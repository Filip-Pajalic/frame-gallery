// Package tv speaks the Samsung Frame art-app protocol over the TV's local
// websocket, and nothing else.
//
// Every call is one short attempt: dial, ask, close. There is no Wake-on-LAN,
// no retry, no keep-alive and no background connection. A TV that is off,
// asleep or off Wi-Fi answers with ErrUnreachable and stays as it was.
//
// Protocol as measured on a 2022 Frame (QE55LS03B, art API 4.3.4.0):
//   - ws://IP:8001/api/v2/channels/com.samsung.art-app?name=<base64>[&token=…]
//     answers ms.channel.connect, then ms.channel.ready.
//   - requests are ms.channel.emit / art_app_request with a JSON string in data;
//     replies are d2d_service_message events whose data carries the same
//     request_id (or id).
//   - D2D sockets (uploads) are announced in conn_info; this TV only answers when
//     conn_info carries request_id.
//
// What the TV sends is parsed into typed replies and refused when it does not
// fit; a reply past a size limit is refused, never cut short.
package tv

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Limits on what is read from and sent to the TV.
const (
	maxMessage   = 4 << 20  // one websocket message; a content list of ~200 items is ~60 KB
	maxUpload    = 64 << 20 // one JPEG; a fitted 3840x2160 picture is 2-6 MB
	maxDeleteIDs = 50       // ids in one delete_image_list
	maxItems     = 5000     // entries in one content list
)

// contentIDRe matches the TV's ids: MY_F0134, SAM-S11112345, MY-C0002.
var contentIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ErrUnreachable means the TV did not answer the dial. It is the normal state
// of a TV that is off or asleep, and is never retried.
var ErrUnreachable = errors.New("TV not reachable")

// ErrBusy means another TV action is still running.
var ErrBusy = errors.New("another TV action is running")

// ErrRefused means the TV answered but refused this client (the Allow prompt
// was declined or timed out, or the client was removed on the TV).
var ErrRefused = errors.New("the TV refused this app (allow it on the TV, then try again)")

// ErrNoAddress means no TV address is configured (FRAME_TV / -tv).
var ErrNoAddress = errors.New("no TV address is set (FRAME_TV)")

// ErrBadContentID refuses an id that cannot be one of the TV's.
var ErrBadContentID = errors.New("not a TV content id")

// Client holds how to reach the TV. It keeps no connection between calls.
type Client struct {
	Host      string        // IP or name of the TV
	Port      int           // 8001 (ws) or 8002 (wss)
	Name      string        // client name shown on the TV
	TokenFile string        // where the TV's token is kept, "" to keep none
	Dial      time.Duration // dial + handshake budget
	Reply     time.Duration // budget for one reply

	mu sync.Mutex // one TV action at a time; a second one gets ErrBusy
}

// Item is one entry of the TV's content list.
type Item struct {
	ContentID  string  `json:"content_id"`
	CategoryID string  `json:"category_id"`
	Matte      string  `json:"matte_id"`
	Width      flexInt `json:"width"`
	Height     flexInt `json:"height"`
}

// flexInt reads a number the TV sends either as 3840 or as "3840".
type flexInt int

func (n *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*n = 0
		return nil
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Errorf("not a number: %s", b)
	}
	*n = flexInt(v)
	return nil
}

// TVError is an error the art app reported for a request.
type TVError struct {
	Request string
	Code    string
}

func (e *TVError) Error() string {
	return fmt.Sprintf("the TV refused %s (error %s)", e.Request, e.Code)
}

// session is one open art channel. It lives for one Client call.
type session struct {
	c  *Client
	ws *websocket.Conn
}

func (c *Client) do(ctx context.Context, fn func(s *session) error) error {
	if c.Host == "" {
		return ErrNoAddress
	}
	if !c.mu.TryLock() {
		return ErrBusy
	}
	defer c.mu.Unlock()
	s, err := c.open(ctx)
	if err != nil {
		return err
	}
	defer s.ws.Close()
	return fn(s)
}

func (c *Client) wsURL() string {
	scheme := "ws"
	if c.Port == 8002 {
		scheme = "wss"
	}
	q := url.Values{}
	q.Set("name", base64.StdEncoding.EncodeToString([]byte(c.Name)))
	if tok := c.token(); tok != "" {
		q.Set("token", tok)
	}
	return fmt.Sprintf("%s://%s/api/v2/channels/com.samsung.art-app?%s", scheme,
		net.JoinHostPort(c.Host, strconv.Itoa(c.Port)), q.Encode())
}

// token reads the kept token; no file means no token yet.
func (c *Client) token() string {
	if c.TokenFile == "" {
		return ""
	}
	b, err := os.ReadFile(c.TokenFile)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("tv: could not read the TV's token: %v", err)
		}
		return ""
	}
	return strings.TrimSpace(string(b))
}

// saveToken keeps the TV's token. A failure is logged, not fatal: the call
// still works, the TV just asks to be allowed again next time.
func (c *Client) saveToken(tok string) {
	if c.TokenFile == "" || tok == "" || tok == c.token() {
		return
	}
	if err := os.WriteFile(c.TokenFile, []byte(tok+"\n"), 0o600); err != nil {
		log.Printf("tv: could not keep the TV's token: %v", err)
	}
}

func (c *Client) open(ctx context.Context) (*session, error) {
	d := websocket.Dialer{
		HandshakeTimeout: c.Dial,
		NetDialContext:   (&net.Dialer{Timeout: c.Dial}).DialContext,
		TLSClientConfig:  &tls.Config{InsecureSkipVerify: true}, // the TV's certificate is self-signed
	}
	dctx, cancel := context.WithTimeout(ctx, c.Dial)
	defer cancel()
	ws, _, err := d.DialContext(dctx, c.wsURL(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	ws.SetReadLimit(maxMessage)
	s := &session{c: c, ws: ws}
	// ms.channel.connect first (it may carry a token), then ms.channel.ready.
	// A new client name waits here while the TV shows its Allow prompt.
	deadline := time.Now().Add(c.Dial + 30*time.Second)
	for {
		f, err := s.read(deadline)
		if err != nil {
			ws.Close()
			return nil, fmt.Errorf("%w: no channel ready: %v", ErrUnreachable, err)
		}
		switch f.Event {
		case "ms.channel.connect":
			var d struct {
				Token string `json:"token"`
			}
			if err := json.Unmarshal(f.Data, &d); err != nil {
				log.Printf("tv: unreadable ms.channel.connect: %v", err)
			}
			c.saveToken(d.Token)
		case "ms.channel.ready":
			return s, nil
		case "ms.channel.unauthorized", "ms.channel.timeOut":
			ws.Close()
			return nil, ErrRefused
		}
	}
}

type frame struct {
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data"`
}

func (s *session) read(deadline time.Time) (frame, error) {
	for {
		if err := s.ws.SetReadDeadline(deadline); err != nil {
			return frame{}, err
		}
		mt, b, err := s.ws.ReadMessage()
		if err != nil {
			return frame{}, err // includes a message over maxMessage
		}
		if mt != websocket.TextMessage {
			continue // binary frames only matter to the old 0.97 API
		}
		var f frame
		if err := json.Unmarshal(b, &f); err != nil {
			return frame{}, fmt.Errorf("the TV sent something that is not JSON: %w", err)
		}
		return f, nil
	}
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error; it panics instead
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// send emits one art_app_request. id and request_id are both set to id.
func (s *session) send(id string, req map[string]any) error {
	req["id"] = id
	req["request_id"] = id
	inner, err := json.Marshal(req)
	if err != nil {
		return err
	}
	msg := map[string]any{
		"method": "ms.channel.emit",
		"params": map[string]any{"event": "art_app_request", "to": "host", "data": string(inner)},
	}
	if err := s.ws.SetWriteDeadline(time.Now().Add(s.c.Reply)); err != nil {
		return err
	}
	return s.ws.WriteJSON(msg)
}

// reply is a d2d_service_message from the art app. Only the fields this
// package reads are declared; the rest of the message is ignored.
type reply struct {
	Event       string          `json:"event"`
	RequestID   string          `json:"request_id"`
	ID          string          `json:"id"`
	ErrorCode   json.RawMessage `json:"error_code"`
	RequestData string          `json:"request_data"`
	Version     string          `json:"version"`
	Value       string          `json:"value"`
	ContentID   string          `json:"content_id"`
	ContentList string          `json:"content_list"`
	ConnInfo    json.RawMessage `json:"conn_info"`
}

// wait reads d2d replies until one matches. With id "" any reply whose event
// is sub matches (image_added carries no request id we can rely on).
func (s *session) wait(id, sub string, budget time.Duration) (reply, error) {
	deadline := time.Now().Add(budget)
	for {
		f, err := s.read(deadline)
		if err != nil {
			return reply{}, fmt.Errorf("no answer from the TV: %w", err)
		}
		if f.Event != "d2d_service_message" {
			continue
		}
		var raw string
		if err := json.Unmarshal(f.Data, &raw); err != nil {
			return reply{}, fmt.Errorf("d2d_service_message without a JSON string: %w", err)
		}
		var r reply
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			return reply{}, fmt.Errorf("unreadable reply from the TV: %w", err)
		}
		rid := r.RequestID
		if rid == "" {
			rid = r.ID
		}
		if id != "" && rid != id {
			continue
		}
		if r.Event == "error" {
			req := "a request"
			var rd struct {
				Request string `json:"request"`
			}
			if json.Unmarshal([]byte(r.RequestData), &rd) == nil && rd.Request != "" {
				req = rd.Request
			}
			return reply{}, &TVError{Request: req, Code: strings.Trim(string(r.ErrorCode), `"`)}
		}
		if sub == "" || r.Event == sub {
			return r, nil
		}
	}
}

func (s *session) ask(req map[string]any) (reply, error) {
	id := newID()
	if err := s.send(id, req); err != nil {
		return reply{}, err
	}
	return s.wait(id, "", s.c.Reply)
}

// Status is what one look at the TV tells.
type Status struct {
	APIVersion string `json:"api_version"`
	ArtMode    string `json:"art_mode"` // "on" / "off"
	Current    string `json:"current"`  // content id on screen
}

// Status reads the API version, art mode and the current artwork.
func (c *Client) Status(ctx context.Context) (Status, error) {
	var st Status
	err := c.do(ctx, func(s *session) error {
		r, err := s.ask(map[string]any{"request": "api_version"})
		if err != nil {
			return err
		}
		if r.Version == "" {
			return errors.New("the TV answered api_version without a version")
		}
		st.APIVersion = r.Version
		if r, err = s.ask(map[string]any{"request": "get_artmode_status"}); err != nil {
			return err
		}
		if r.Value != "on" && r.Value != "off" {
			return fmt.Errorf("the TV reported art mode %q", r.Value)
		}
		st.ArtMode = r.Value
		if r, err = s.ask(map[string]any{"request": "get_current_artwork"}); err != nil {
			return err
		}
		st.Current = r.ContentID
		return nil
	})
	return st, err
}

// List returns everything in the TV's art store (uploads and Art Store items).
func (c *Client) List(ctx context.Context) ([]Item, error) {
	var items []Item
	err := c.do(ctx, func(s *session) error {
		r, err := s.ask(map[string]any{"request": "get_content_list", "category": nil})
		if err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(r.ContentList), &items); err != nil {
			return fmt.Errorf("the TV's content list is unreadable: %w", err)
		}
		if len(items) > maxItems {
			return fmt.Errorf("the TV listed %d items, more than the %d this app accepts", len(items), maxItems)
		}
		for _, it := range items {
			if !contentIDRe.MatchString(it.ContentID) {
				return fmt.Errorf("the TV's content list holds %w: %q", ErrBadContentID, it.ContentID)
			}
		}
		return nil
	})
	return items, err
}

// Show puts an artwork on screen.
func (c *Client) Show(ctx context.Context, contentID string) error {
	if !contentIDRe.MatchString(contentID) {
		return fmt.Errorf("%w: %q", ErrBadContentID, contentID)
	}
	return c.do(ctx, func(s *session) error {
		_, err := s.ask(map[string]any{"request": "select_image", "category_id": nil, "content_id": contentID, "show": true})
		return err
	})
}

// SetArtMode turns art mode on or off. It acts only on a TV that already
// answers; it cannot and does not power one on.
func (c *Client) SetArtMode(ctx context.Context, on bool) error {
	v := "off"
	if on {
		v = "on"
	}
	return c.do(ctx, func(s *session) error {
		_, err := s.ask(map[string]any{"request": "set_artmode_status", "value": v})
		return err
	})
}

// Delete removes artworks from the TV, at most maxDeleteIDs at a time.
func (c *Client) Delete(ctx context.Context, ids ...string) error {
	if len(ids) == 0 {
		return nil
	}
	if len(ids) > maxDeleteIDs {
		return fmt.Errorf("%d ids in one delete; the limit is %d", len(ids), maxDeleteIDs)
	}
	list := make([]map[string]string, len(ids))
	for i, id := range ids {
		if !contentIDRe.MatchString(id) {
			return fmt.Errorf("%w: %q", ErrBadContentID, id)
		}
		list[i] = map[string]string{"content_id": id}
	}
	return c.do(ctx, func(s *session) error {
		_, err := s.ask(map[string]any{"request": "delete_image_list", "content_id_list": list})
		return err
	})
}

// Upload sends one JPEG with matte "none" and returns the TV's content id.
func (c *Client) Upload(ctx context.Context, jpeg []byte) (string, error) {
	if len(jpeg) < 4 || jpeg[0] != 0xFF || jpeg[1] != 0xD8 {
		return "", errors.New("upload refused: not a JPEG")
	}
	if len(jpeg) > maxUpload {
		return "", fmt.Errorf("upload refused: %d MB is over the %d MB limit", len(jpeg)>>20, maxUpload>>20)
	}
	var cid string
	err := c.do(ctx, func(s *session) error {
		id := newID()
		err := s.send(id, map[string]any{
			"request":           "send_image",
			"file_type":         "jpg",
			"file_size":         len(jpeg),
			"image_date":        time.Now().Format("2006:01:02 15:04:05"),
			"matte_id":          "none",
			"portrait_matte_id": "none",
			"conn_info":         map[string]any{"d2d_mode": "socket", "request_id": id, "id": id},
		})
		if err != nil {
			return err
		}
		ready, err := s.wait(id, "ready_to_use", s.c.Reply)
		if err != nil {
			return err
		}
		ci, err := connInfo(ready.ConnInfo)
		if err != nil {
			return err
		}
		if err := s.c.d2dSend(ci, jpeg); err != nil {
			return fmt.Errorf("sending the image: %w", err)
		}
		done, err := s.wait("", "image_added", 90*time.Second)
		if err != nil {
			return err
		}
		if !contentIDRe.MatchString(done.ContentID) {
			return fmt.Errorf("the TV gave the image %w: %q", ErrBadContentID, done.ContentID)
		}
		cid = done.ContentID
		return nil
	})
	return cid, err
}

type d2dInfo struct {
	IP      string  `json:"ip"`
	Port    flexInt `json:"port"`
	Key     string  `json:"key"`
	Secured bool    `json:"secured"`
}

// connInfo reads conn_info, which the TV sends as a JSON string holding JSON
// (older firmware sends the object itself).
func connInfo(raw json.RawMessage) (d2dInfo, error) {
	var ci d2dInfo
	if len(raw) == 0 || string(raw) == "null" {
		return ci, errors.New("the TV sent no conn_info")
	}
	body := []byte(raw)
	var inner string
	if json.Unmarshal(raw, &inner) == nil {
		body = []byte(inner)
	}
	if err := json.Unmarshal(body, &ci); err != nil {
		return ci, fmt.Errorf("conn_info: %w", err)
	}
	if net.ParseIP(ci.IP) == nil || ci.Port <= 0 || ci.Port > 65535 {
		return ci, fmt.Errorf("conn_info has no usable address (%q port %d)", ci.IP, ci.Port)
	}
	return ci, nil
}

func (c *Client) d2dSend(ci d2dInfo, data []byte) error {
	addr := net.JoinHostPort(ci.IP, strconv.Itoa(int(ci.Port)))
	nd := &net.Dialer{Timeout: c.Dial}
	var conn net.Conn
	var err error
	if ci.Secured {
		conn, err = tls.DialWithDialer(nd, "tcp", addr, &tls.Config{InsecureSkipVerify: true})
	} else {
		conn, err = nd.Dial("tcp", addr)
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(120 * time.Second)); err != nil {
		return err
	}
	hdr, err := json.Marshal(map[string]any{
		"num": 0, "total": 1, "fileLength": len(data), "fileName": "image",
		"fileType": "jpg", "secKey": ci.Key, "version": "0.0.1",
	})
	if err != nil {
		return err
	}
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(hdr)))
	for _, b := range [][]byte{n[:], hdr, data} {
		if _, err := conn.Write(b); err != nil {
			return err
		}
	}
	return nil
}
