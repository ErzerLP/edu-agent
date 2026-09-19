// Package companion 定义可选本地通道；不包含任何 OS 执行器或默认工具。
package companion

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"time"
)

const MaxPayload = 128 << 10
const Lease = 45 * time.Second

var ErrDenied = errors.New("companion_not_authorized")
var ErrConflict = errors.New("companion_operation_conflict")
var ErrLimit = errors.New("companion_capacity")

type Device struct {
	ID        string `json:"id"`
	Host      string `json:"host"`
	User      string `json:"user"`
	OS        string `json:"os"`
	Workspace string `json:"workspace"`
}

type Grant struct {
	Generation   int64  `json:"generation"`
	Space        string `json:"space"`
	Conversation string `json:"conversation"`
	Files        bool   `json:"files"`
	Shell        bool   `json:"shell"`
	Model        bool   `json:"model"`
	Destination  string `json:"destination"`
}

type Operation struct {
	ID        string          `json:"id"`
	Run       string          `json:"run"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
}

type Receipt struct {
	Task         string          `json:"task,omitempty"`
	TaskRun      string          `json:"task_run,omitempty"`
	ID           string          `json:"id"`
	Device       string          `json:"device"`
	Conversation string          `json:"conversation"`
	Run          string          `json:"run"`
	State        string          `json:"state"`
	Value        json.RawMessage `json:"value,omitempty"`
	Error        string          `json:"error,omitempty"`
}

type View struct {
	ID       string    `json:"id"`
	State    string    `json:"state"`
	Device   Device    `json:"device"`
	Grant    *Grant    `json:"grant,omitempty"`
	Receipts []Receipt `json:"receipts"`
}

type Pair struct {
	ID     string `json:"id"`
	Code   string `json:"code"`
	Secret string `json:"secret"`
}

type Attach struct {
	ID     string `json:"id"`
	Code   string `json:"code"`
	Device Device `json:"device"`
}

type Channel struct {
	Token string `json:"token"`
}

type Exchange struct {
	Receipt *Receipt `json:"receipt,omitempty"`
}

type Delivery struct {
	Grant     *Grant     `json:"grant,omitempty"`
	Operation *Operation `json:"operation,omitempty"`
}

func Secret() string { return rand.Text() + rand.Text() }

// MAC 同时绑定 Origin、通道、单调序号和完整正文，禁止跨通道及载荷替换。
func MAC(token, origin, id string, sequence uint64, body []byte) string {
	m := hmac.New(sha256.New, []byte(token))
	m.Write([]byte(origin + "\n" + id + "\n" + strconv.FormatUint(sequence, 10) + "\n"))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

func Allowed(g Grant, tool string, model bool) bool {
	if model && !g.Model {
		return false
	}
	switch tool {
	case "list", "read", "stat", "prepare_write", "prepare_edit":
		return g.Files
	case "commit", "discard":
		return g.Files && !model
	case "shell", "task":
		return g.Shell
	}
	return false
}
