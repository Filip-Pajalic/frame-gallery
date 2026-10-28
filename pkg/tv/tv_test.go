package tv

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeTV answers like the 2022 Frame does, including the D2D upload socket.
type fakeTV struct {
	t        *testing.T
	d2d      net.Listener
	got      []byte // last upload
	dials    atomic.Int32
	lastConn map[string]any // conn_info of the last send_image
	mode     string         // "noversion", "huge", "badid": misbehave on purpose
}

func newFakeTV(t *testing.T) (*fakeTV, *Client) {
	f := &fakeTV{t: t}
	var err error
	f.d2d, err = tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.d2d.Close() })
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	p, _ := strconv.Atoi(port)
	return f, &Client{Host: host, Port: p, Name: "Frame Gallery", Dial: 2 * time.Second, Reply: 2 * time.Second}
}

func (f *fakeTV) serve(w http.ResponseWriter, r *http.Request) {
	f.dials.Add(1)
	if r.URL.Path != "/api/v2/channels/com.samsung.art-app" || r.URL.Query().Get("name") == "" {
		http.NotFound(w, r)
		return
	}
	ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	ws.WriteJSON(map[string]any{"event": "ms.channel.connect", "data": map[string]any{"id": "x"}})
	ws.WriteJSON(map[string]any{"event": "ms.channel.ready", "data": map[string]any{}})
	for {
		var msg struct {
			Method string `json:"method"`
			Params struct {
				Event string `json:"event"`
				Data  string `json:"data"`
			} `json:"params"`
		}
		if err := ws.ReadJSON(&msg); err != nil {
			return
		}
		var req map[string]any
		json.Unmarshal([]byte(msg.Params.Data), &req)
		id, _ := req["request_id"].(string)
		answer := func(p map[string]any) {
			p["request_id"] = id
			p["id"] = id
			b, _ := json.Marshal(p)
			ws.WriteJSON(map[string]any{"event": "d2d_service_message", "data": string(b)})
		}
		// an unrelated event first, as the TV sends them unasked
		ws.WriteJSON(map[string]any{"event": "d2d_service_message", "data": `{"event":"go_to_standby","id":"other"}`})
		switch {
		case f.mode == "huge":
			ws.WriteMessage(websocket.TextMessage, []byte(`{"event":"d2d_service_message","data":"`+strings.Repeat("x", maxMessage)+`"}`))
			continue
		case f.mode == "noversion" && req["request"] == "api_version":
			answer(map[string]any{"event": "api_version"})
			continue
		case f.mode == "badid" && req["request"] == "get_content_list":
			answer(map[string]any{"event": "get_content_list", "content_list": `[{"content_id":"../etc/passwd"}]`})
			continue
		}
		switch req["request"] {
		case "api_version":
			answer(map[string]any{"event": "api_version", "version": "4.3.4.0"})
		case "get_artmode_status":
			answer(map[string]any{"event": "art_mode_changed", "value": "on"})
		case "get_current_artwork":
			answer(map[string]any{"event": "get_current_artwork", "content_id": "MY_F0134"})
		case "get_content_list":
			answer(map[string]any{"event": "get_content_list", "content_list": `[{"content_id":"MY_F0134","category_id":"MY-C0002","matte_id":"none","width":"3840","height":"2160"},{"content_id":"SAM-S1111","category_id":"MY-C0004"}]`})
		case "select_image":
			if req["content_id"] == "MISSING" {
				answer(map[string]any{"event": "error", "error_code": "-1", "request_data": `{"request":"select_image"}`})
				continue
			}
			answer(map[string]any{"event": "select_image"})
		case "delete_image_list":
			answer(map[string]any{"event": "image_deleted", "content_id_list": `[]`})
		case "set_artmode_status":
			answer(map[string]any{"event": "art_mode_changed", "value": req["value"]})
		case "send_image":
			ci, _ := req["conn_info"].(map[string]any)
			f.lastConn = ci
			if req["matte_id"] != "none" {
				answer(map[string]any{"event": "error", "error_code": "-10"})
				continue
			}
			_, port, _ := net.SplitHostPort(f.d2d.Addr().String())
			info, _ := json.Marshal(map[string]any{"ip": "127.0.0.1", "port": port, "key": "sekrit", "secured": true})
			answer(map[string]any{"event": "ready_to_use", "conn_info": string(info)})
			size := int(req["file_size"].(float64))
			c, err := f.d2d.Accept()
			if err != nil {
				return
			}
			var n [4]byte
			io.ReadFull(c, n[:])
			hdr := make([]byte, binary.BigEndian.Uint32(n[:]))
			io.ReadFull(c, hdr)
			var h map[string]any
			json.Unmarshal(hdr, &h)
			if h["secKey"] != "sekrit" || int(h["fileLength"].(float64)) != size {
				f.t.Errorf("bad D2D header %s", hdr)
			}
			f.got = make([]byte, size)
			io.ReadFull(c, f.got)
			c.Close()
			b, _ := json.Marshal(map[string]any{"event": "image_added", "content_id": "MY_F0200"})
			ws.WriteJSON(map[string]any{"event": "d2d_service_message", "data": string(b)})
		}
	}
}

func TestStatusListShow(t *testing.T) {
	_, c := newFakeTV(t)
	ctx := context.Background()
	st, err := c.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.APIVersion != "4.3.4.0" || st.ArtMode != "on" || st.Current != "MY_F0134" {
		t.Fatalf("status %+v", st)
	}
	items, err := c.List(ctx)
	if err != nil || len(items) != 2 || items[0].ContentID != "MY_F0134" || items[0].Width != 3840 {
		t.Fatalf("list %+v %v", items, err)
	}
	if err := c.Show(ctx, "MY_F0134"); err != nil {
		t.Fatal(err)
	}
	var te *TVError
	if err := c.Show(ctx, "MISSING"); !errors.As(err, &te) || te.Request != "select_image" {
		t.Fatalf("want TVError, got %v", err)
	}
	if err := c.SetArtMode(ctx, false); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, "MY_F0134"); err != nil {
		t.Fatal(err)
	}
}

func TestUpload(t *testing.T) {
	f, c := newFakeTV(t)
	img := append([]byte{0xFF, 0xD8}, strings.Repeat("jpegdata", 10000)...)
	cid, err := c.Upload(context.Background(), img)
	if err != nil {
		t.Fatal(err)
	}
	if cid != "MY_F0200" || string(f.got) != string(img) {
		t.Fatalf("cid %q, %d bytes", cid, len(f.got))
	}
	if f.lastConn["request_id"] == nil || f.lastConn["request_id"] == "" {
		t.Fatalf("conn_info lacks request_id: %v", f.lastConn)
	}
}

// A TV that does not answer is tried exactly once and reported as unreachable.
func TestUnreachableIsOneAttempt(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().(*net.TCPAddr)
	l.Close() // nothing listens there now
	c := &Client{Host: "127.0.0.1", Port: addr.Port, Name: "x", Dial: 500 * time.Millisecond, Reply: time.Second}
	t0 := time.Now()
	_, err := c.Status(context.Background())
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("want ErrUnreachable, got %v", err)
	}
	if d := time.Since(t0); d > 2*time.Second {
		t.Fatalf("took %v: looks like a retry", d)
	}
}

func TestBusy(t *testing.T) {
	c := &Client{Host: "tv.invalid"}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.Status(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatalf("want ErrBusy, got %v", err)
	}
}

// selfSigned makes a throwaway certificate, like the TV's own.
func selfSigned(t *testing.T) tls.Certificate {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// What the TV sends that does not fit is refused, not read past.
func TestRefusesMalformedReplies(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"noversion", "huge"} {
		f, c := newFakeTV(t)
		f.mode = mode
		if _, err := c.Status(ctx); err == nil {
			t.Errorf("%s: status accepted", mode)
		}
	}
	f, c := newFakeTV(t)
	f.mode = "badid"
	if _, err := c.List(ctx); !errors.Is(err, ErrBadContentID) {
		t.Errorf("badid: want ErrBadContentID, got %v", err)
	}
}

// Requests past a bound are refused before the TV is dialled.
func TestRefusesAtBounds(t *testing.T) {
	f, c := newFakeTV(t)
	ctx := context.Background()
	if err := c.Show(ctx, "x/../y"); !errors.Is(err, ErrBadContentID) {
		t.Errorf("show: want ErrBadContentID, got %v", err)
	}
	ids := make([]string, maxDeleteIDs+1)
	for i := range ids {
		ids[i] = "MY_F0001"
	}
	if err := c.Delete(ctx, ids...); err == nil {
		t.Error("delete of too many ids accepted")
	}
	if _, err := c.Upload(ctx, []byte("not a jpeg")); err == nil {
		t.Error("non-JPEG upload accepted")
	}
	if _, err := c.Upload(ctx, append([]byte{0xFF, 0xD8}, make([]byte, maxUpload)...)); err == nil {
		t.Error("oversize upload accepted")
	}
	if n := f.dials.Load(); n != 0 {
		t.Errorf("refused calls dialled the TV %d times", n)
	}
}
