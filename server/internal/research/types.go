// Package research 保存隔离候选和可核对片段，不拥有正式知识或工具权限。
package research

import (
	"errors"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/server/internal/pdfsource"
)

const MaxSources = 4
const MaxText = 16000

var ErrPolicy = errors.New("research_policy_rejected")

type Policy struct {
	Mode    string   `json:"mode"`
	Domains []string `json:"domains"`
}

type Request struct {
	Topic           string `json:"topic"`
	ExternalConsent bool   `json:"external_consent"`
	AutoAdopt       bool   `json:"auto_adopt"`
	Policy          Policy `json:"policy"`
}

func (r Request) Validate() error {
	if !r.ExternalConsent || strings.TrimSpace(r.Topic) == "" || len(r.Topic) > 300 || !utf8.ValidString(r.Topic) || strings.ContainsAny(r.Topic, "\x00\r\n") {
		return ErrPolicy
	}
	if r.Policy.Mode != "supplement" && r.Policy.Mode != "prefer" && r.Policy.Mode != "restrict" {
		return ErrPolicy
	}
	if r.Policy.Domains == nil || len(r.Policy.Domains) > 5 || r.Policy.Mode != "supplement" && len(r.Policy.Domains) == 0 {
		return ErrPolicy
	}
	for _, domain := range r.Policy.Domains {
		if domain != strings.ToLower(domain) || strings.ContainsAny(domain, "/:@?*\\ %\r\n") || !strings.Contains(domain, ".") || len(domain) > 253 {
			return ErrPolicy
		}
		if _, err := ValidateURL("https://" + domain); err != nil {
			return ErrPolicy
		}
	}
	return nil
}

func (p Policy) Allows(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if p.Mode != "restrict" {
		return true
	}
	for _, domain := range p.Domains {
		if strings.EqualFold(u.Hostname(), domain) {
			return true
		}
	}
	return false
}

// 限制政策在提供商请求前转为查询约束，候选与每跳抓取还会执行同一政策。
func (r Request) Query() string {
	if r.Policy.Mode == "supplement" {
		return r.Topic
	}
	parts := make([]string, len(r.Policy.Domains))
	for i, domain := range r.Policy.Domains {
		parts[i] = "site:" + domain
	}
	return "(" + strings.Join(parts, " OR ") + ") (" + r.Topic + ")"
}

type Fragment struct {
	Page  int    `json:"page,omitempty"`
	ID    string `json:"id"`
	Start int    `json:"start"`
	End   int    `json:"end"`
	Text  string `json:"text"`
}

type Source struct {
	DocumentRevisionID  string            `json:"document_revision_id,omitempty"`
	PDF                 *pdfsource.Report `json:"pdf,omitempty"`
	PDFOriginal         []byte            `json:"-"`
	SpaceID             string            `json:"space_id"`
	GoalID              string            `json:"goal_id"`
	Purpose             string            `json:"purpose"`
	ID                  string            `json:"id"`
	RevisionID          string            `json:"revision_id"`
	Locator             string            `json:"locator"`
	FinalURL            string            `json:"final_url"`
	Title               string            `json:"title"`
	Kind                string            `json:"kind"`
	Status              string            `json:"status"`
	Failure             string            `json:"failure"`
	FetchedAt           *time.Time        `json:"fetched_at,omitempty"`
	Fingerprint         string            `json:"fingerprint"`
	Parser              string            `json:"parser"`
	Coverage            string            `json:"coverage"`
	StorageAllowed      bool              `json:"storage_allowed"`
	Text                string            `json:"text"`
	Fragments           []Fragment        `json:"fragments"`
	KnowledgeRevisionID string            `json:"knowledge_revision_id"`
	CollectionID        string            `json:"collection_id"`
}

type Citation struct {
	SourceID   string `json:"source_id"`
	RevisionID string `json:"revision_id"`
	FragmentID string `json:"fragment_id"`
	Quote      string `json:"quote"`
}
type Point struct {
	Text      string     `json:"text"`
	Citations []Citation `json:"citations"`
}
type Synthesis struct {
	Points   []Point  `json:"points"`
	Gaps     []string `json:"gaps"`
	Examples []string `json:"examples"`
}
type State struct {
	Request             Request    `json:"request"`
	Discovered          bool       `json:"discovered"`
	Sources             []Source   `json:"sources"`
	Synthesis           *Synthesis `json:"synthesis,omitempty"`
	SearchConfiguration string     `json:"search_configuration"`
}

// 引用仅接受已读取原文中的真实片段；不允许模型用 URL 或自造片段替代。
func (s Synthesis) Validate(sources []Source) error {
	if len(s.Points) > 12 || len(s.Gaps) > 12 || len(s.Examples) > 6 {
		return ErrPolicy
	}
	for _, p := range s.Points {
		if strings.TrimSpace(p.Text) == "" || len(p.Text) > 2000 || len(p.Citations) == 0 || len(p.Citations) > 8 {
			return ErrPolicy
		}
		for _, c := range p.Citations {
			found := false
			for _, source := range sources {
				if source.ID != c.SourceID || source.RevisionID != c.RevisionID {
					continue
				}
				for _, fragment := range source.Fragments {
					if fragment.ID == c.FragmentID && c.Quote != "" && strings.Contains(fragment.Text, c.Quote) && fragment.Start >= 0 && fragment.End <= len(source.Text) && source.Text[fragment.Start:fragment.End] == fragment.Text {
						found = true
					}
				}
			}
			if !found {
				return ErrPolicy
			}
		}
	}
	for _, text := range append(append([]string{}, s.Gaps...), s.Examples...) {
		if len(text) > 2000 {
			return ErrPolicy
		}
	}
	return nil
}
