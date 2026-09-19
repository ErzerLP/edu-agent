package companion

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"sync"
	"time"
)

type record struct {
	receipt Receipt
	digest  [32]byte
}

type connection struct {
	owner, browserDevice, secret, code, token string
	view                                      View
	created, browserAt, nativeAt              time.Time
	sequence                                  uint64
	revoked                                   bool
	queue                                     *Operation
	records                                   map[string]*record
	order                                     []string
	bytes                                     int
}

// Broker 仅在内存中中转；重启即失效，不恢复授权、参数或执行。
type Broker struct {
	mu          sync.Mutex
	origin      string
	now         func() time.Time
	connections map[string]*connection
}

func New(origin string) *Broker {
	return &Broker{origin: origin, now: time.Now, connections: map[string]*connection{}}
}

func (b *Broker) Create(owner, browserDevice string) (Pair, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, c := range b.connections {
		if b.now().Sub(c.browserAt) > 5*time.Minute {
			delete(b.connections, id)
		}
	}
	if owner == "" || browserDevice == "" {
		return Pair{}, ErrDenied
	}
	if len(b.connections) >= 16 {
		return Pair{}, ErrLimit
	}
	p := Pair{ID: Secret(), Code: Secret(), Secret: Secret()}
	b.connections[p.ID] = &connection{owner: owner, browserDevice: browserDevice, secret: p.Secret, code: p.Code,
		created: b.now(), browserAt: b.now(), view: View{ID: p.ID, State: "pairing", Receipts: []Receipt{}}, records: map[string]*record{}}
	return p, nil
}

func (b *Broker) Attach(a Attach) (Channel, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.connections[a.ID]
	if c == nil || c.revoked || c.code == "" || !hmac.Equal([]byte(a.Code), []byte(c.code)) || b.now().Sub(c.created) > 5*time.Minute ||
		a.Device.ID == "" || a.Device.Host == "" || a.Device.User == "" || a.Device.Workspace == "" || (a.Device.OS != "linux" && a.Device.OS != "darwin") {
		return Channel{}, ErrDenied
	}
	if len(a.Device.ID) > 104 || len(a.Device.Host) > 256 || len(a.Device.User) > 256 || len(a.Device.Workspace) > 4096 {
		return Channel{}, ErrDenied
	}
	c.code, c.token, c.nativeAt = "", Secret(), b.now()
	c.view.Device, c.view.State = a.Device, "awaiting_authorization"
	return Channel{Token: c.token}, nil
}

func (b *Broker) browser(owner, id, secret string) (*connection, error) {
	c := b.connections[id]
	if c == nil || c.owner != owner || !hmac.Equal([]byte(c.secret), []byte(secret)) {
		return nil, ErrDenied
	}
	if c.view.Grant != nil && b.now().Sub(c.browserAt) > Lease {
		b.revoke(c)
	}
	return c, nil
}

func (b *Broker) revoke(c *connection) {
	c.revoked, c.queue, c.view.State = true, nil, "revoked"
	for _, r := range c.records {
		r.receipt.Value = nil
		if r.receipt.State == "queued" {
			r.receipt.State = "not_executed"
		}
		if r.receipt.State == "dispatched" {
			r.receipt.State = "unknown"
		}
	}
}

func (b *Broker) Verify(id string, sequence uint64, mac string, body []byte) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.connections[id]
	return c != nil && !c.revoked && c.token != "" && sequence > c.sequence && hmac.Equal([]byte(mac), []byte(MAC(c.token, b.origin, id, sequence, body)))
}

func (b *Broker) Revoke(owner, id, secret string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, err := b.browser(owner, id, secret)
	if err != nil {
		return err
	}
	b.revoke(c)
	return nil
}

func (b *Broker) Authorize(owner, id, secret, device string, g Grant) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, err := b.browser(owner, id, secret)
	if err != nil {
		return err
	}
	if c.revoked || c.token == "" || c.view.Device.ID != device || c.view.Grant != nil || g.Space == "" || g.Conversation == "" || (!g.Files && !g.Shell) || g.Model && g.Destination == "" || b.now().Sub(c.nativeAt) > Lease {
		return ErrDenied
	}
	// 同一个浏览器对话只接受一个设备授权，旧授权必须先撤销。
	for _, other := range b.connections {
		if !other.revoked && other.view.Grant != nil && other.browserDevice == c.browserDevice && other.view.Grant.Conversation == g.Conversation {
			return ErrConflict
		}
	}
	c.view.Grant, c.browserAt, c.view.State = &g, b.now(), "connected"
	return nil
}

func (b *Broker) View(owner, id, secret string) (View, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, err := b.browser(owner, id, secret)
	if err != nil {
		return View{}, err
	}
	if !c.revoked {
		c.browserAt = b.now()
	}
	v := c.view
	if v.Grant != nil {
		g := *v.Grant
		v.Grant = &g
	}
	if !c.revoked && c.token != "" && b.now().Sub(c.nativeAt) > 5*time.Second {
		v.State = "disconnected"
	}
	v.Receipts = []Receipt{}
	for _, id := range c.order[max(0, len(c.order)-20):] {
		r := c.records[id].receipt
		if r.State == "dispatched" && v.State == "disconnected" {
			r.State = "unknown"
		}
		r.Value = append(json.RawMessage(nil), r.Value...)
		v.Receipts = append(v.Receipts, r)
	}
	return v, nil
}

func (b *Broker) Submit(owner, id, secret string, op Operation) (Receipt, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, err := b.browser(owner, id, secret)
	if err != nil {
		return Receipt{}, err
	}
	return b.submit(c, op, false)
}

func (b *Broker) submit(c *connection, op Operation, model bool) (Receipt, error) {
	if c.revoked || c.view.Grant == nil || b.now().Sub(c.browserAt) > Lease || b.now().Sub(c.nativeAt) > Lease || !Allowed(*c.view.Grant, op.Tool, model) {
		return Receipt{}, ErrDenied
	}
	if len(op.ID) < 1 || len(op.ID) > 160 || len(op.Run) > 160 || len(op.Arguments) > MaxPayload/2 || !json.Valid(op.Arguments) {
		return Receipt{}, ErrDenied
	}
	raw, _ := json.Marshal(op)
	digest := sha256.Sum256(raw)
	if r := c.records[op.ID]; r != nil {
		if r.digest != digest {
			return Receipt{}, ErrConflict
		}
		return cloneReceipt(r.receipt), nil
	}
	if c.queue != nil || len(c.records) >= 1024 || c.bytes >= 8<<20 {
		return Receipt{}, ErrLimit
	}
	r := Receipt{ID: op.ID, Device: c.view.Device.ID, Conversation: c.view.Grant.Conversation, Run: op.Run, State: "queued"}
	c.records[op.ID] = &record{receipt: r, digest: digest}
	c.order = append(c.order, op.ID)
	op.Arguments = append(json.RawMessage(nil), op.Arguments...)
	c.queue = &op
	return r, nil
}

func cloneReceipt(r Receipt) Receipt { r.Value = append(json.RawMessage(nil), r.Value...); return r }

func (b *Broker) Receipt(owner, id, secret, operation string) (Receipt, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, err := b.browser(owner, id, secret)
	if err != nil {
		return Receipt{}, err
	}
	r := c.records[operation]
	if r == nil {
		return Receipt{}, ErrDenied
	}
	return cloneReceipt(r.receipt), nil
}

// Exchange 领取即消费。响应丢失也绝不再次投递；本机可重复上传同一回执。
func (b *Broker) Exchange(id string, sequence uint64, mac string, body []byte) (Delivery, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.connections[id]
	if c == nil || c.token == "" || sequence <= c.sequence || !hmac.Equal([]byte(mac), []byte(MAC(c.token, b.origin, id, sequence, body))) {
		return Delivery{}, ErrDenied
	}
	c.sequence = sequence
	if c.revoked || b.now().Sub(c.browserAt) > Lease {
		b.revoke(c)
		return Delivery{}, ErrDenied
	}
	var e Exchange
	if json.Unmarshal(body, &e) != nil {
		return Delivery{}, ErrDenied
	}
	if e.Receipt != nil {
		r := c.records[e.Receipt.ID]
		if r == nil || (r.receipt.State != "dispatched" && r.receipt.State != "completed") || e.Receipt.Device != c.view.Device.ID || c.view.Grant == nil || e.Receipt.Conversation != c.view.Grant.Conversation || e.Receipt.Run != r.receipt.Run || e.Receipt.State != "completed" || len(e.Receipt.Value) > MaxPayload/2 {
			return Delivery{}, ErrDenied
		}
		if r.receipt.State != "completed" {
			if c.bytes+len(e.Receipt.Value) > 8<<20 {
				r.receipt.State = "unknown"
				r.receipt.Error = "receipt_storage_limit"
			} else {
				r.receipt = cloneReceipt(*e.Receipt)
				c.bytes += len(r.receipt.Value)
			}
		}
	}
	c.nativeAt = b.now()
	d := Delivery{}
	if c.view.Grant != nil {
		g := *c.view.Grant
		d.Grant = &g
	}
	if c.queue != nil {
		op := *c.queue
		op.Arguments = append(json.RawMessage(nil), op.Arguments...)
		d.Operation = &op
		c.records[op.ID].receipt.State = "dispatched"
		c.queue = nil
	}
	return d, nil
}

func (b *Broker) ModelConnection(owner, device, conversation, destination string) string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, c := range b.connections {
		g := c.view.Grant
		if !c.revoked && g != nil && g.Model && c.owner == owner && c.browserDevice == device && g.Conversation == conversation && g.Destination == destination && b.now().Sub(c.browserAt) < Lease && b.now().Sub(c.nativeAt) < Lease {
			return id
		}
	}
	return ""
}

func (b *Broker) ModelSubmit(id, owner, device, conversation, destination string, op Operation) (Receipt, error) {
	if b == nil {
		return Receipt{}, ErrDenied
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.connections[id]
	if c == nil || c.view.Grant == nil || c.owner != owner || c.browserDevice != device || c.view.Grant.Conversation != conversation || c.view.Grant.Destination != destination {
		return Receipt{}, ErrDenied
	}
	return b.submit(c, op, true)
}

func (b *Broker) ModelReceipt(id, operation string) (Receipt, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.connections[id]
	if c == nil || c.revoked || b.now().Sub(c.browserAt) > Lease {
		return Receipt{}, ErrDenied
	}
	r := c.records[operation]
	if r == nil {
		return Receipt{}, ErrDenied
	}
	return cloneReceipt(r.receipt), nil
}

// Principal 供宿主在每次原生轮询时重检学习身份、撤销和隐私代次。
func (b *Broker) Principal(id string) (string, string, *Grant, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.connections[id]
	if c == nil || c.revoked {
		return "", "", nil, ErrDenied
	}
	var g *Grant
	if c.view.Grant != nil {
		copy := *c.view.Grant
		g = &copy
	}
	return c.owner, c.browserDevice, g, nil
}

func (b *Broker) RevokeChannel(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if c := b.connections[id]; c != nil {
		b.revoke(c)
	}
}
