package model

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Identity 是 git 兼容的签名（author/committer）。
type Identity struct {
	Name  string
	Email string
	When  time.Time
}

// Format 渲染为 git 签名："Name <email> <unix> <±hhmm>"。
func (id Identity) Format() string {
	_, offset := id.When.Zone()
	sign := "+"
	if offset < 0 {
		sign, offset = "-", -offset
	}
	return fmt.Sprintf("%s <%s> %d %s%02d%02d",
		id.Name, id.Email, id.When.Unix(), sign, offset/3600, (offset%3600)/60)
}

// RootAnchor 是根锚点；根时间线语义等同 git branch（docs/01 §2）。
const RootAnchor AnchorPath = "/"

// AnchoredCommit 是 banyan 的提交记录（docs/01 §2）：
// tree_oid 是 anchor_path 处的 tree——任意 tree，不限仓库根。
//
// 编码规则（保证 git 兼容）：
//   - anchor == "/" 时不写 anchor 头，序列化体与 git commit 逐字节一致；
//   - anchor != "/" 时在 tree 行后追加 "banyan-anchor <path>" 头，
//     因此 OID 空间与 git 对象天然区分、永不冲突。
type AnchoredCommit struct {
	Anchor     AnchorPath
	TreeOID    OID
	Parents    []OID
	Author     Identity
	Committer  Identity
	Message    string
	Synthetic  bool // 组装算法生成（携带 banyan-synthetic trailer）
	ProjectedFrom OID // 非零表示这是从根时间线 split 出的投影 commit
}

// Encode 渲染 git 兼容的 commit 对象体。
func (c *AnchoredCommit) Encode() []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "tree %s\n", c.TreeOID)
	if c.Anchor != RootAnchor && c.Anchor != "" {
		fmt.Fprintf(&b, "banyan-anchor %s\n", c.Anchor)
	}
	for _, p := range c.Parents {
		fmt.Fprintf(&b, "parent %s\n", p)
	}
	fmt.Fprintf(&b, "author %s\n", c.Author.Format())
	fmt.Fprintf(&b, "committer %s\n", c.Committer.Format())
	if c.ProjectedFrom != "" {
		fmt.Fprintf(&b, "banyan-projected-from %s\n", c.ProjectedFrom)
	}
	if c.Synthetic {
		b.WriteString("banyan-synthetic true\n")
	}
	b.WriteString("\n")
	b.WriteString(c.Message)
	return []byte(b.String())
}

func (c *AnchoredCommit) OID(algo HashAlgo) OID {
	return ObjectOID(CommitType, c.Encode(), algo)
}

// DecodeCommit 解析 commit 对象体（git 格式 + banyan 扩展头）。
func DecodeCommit(content []byte) (*AnchoredCommit, error) {
	c := &AnchoredCommit{}
	header, message, ok := strings.Cut(string(content), "\n\n")
	if !ok {
		return nil, fmt.Errorf("decode commit: missing header terminator")
	}
	c.Message = message
	for _, line := range strings.Split(header, "\n") {
		key, val, found := strings.Cut(line, " ")
		if !found {
			return nil, fmt.Errorf("decode commit: malformed header %q", line)
		}
		switch key {
		case "tree":
			c.TreeOID = OID(val)
		case "banyan-anchor":
			p, err := ParseAnchorPath(val)
			if err != nil {
				return nil, fmt.Errorf("decode commit: %w", err)
			}
			c.Anchor = p
		case "parent":
			c.Parents = append(c.Parents, OID(val))
		case "author":
			id, err := parseIdentity(val)
			if err != nil {
				return nil, fmt.Errorf("decode commit: author: %w", err)
			}
			c.Author = id
		case "committer":
			id, err := parseIdentity(val)
			if err != nil {
				return nil, fmt.Errorf("decode commit: committer: %w", err)
			}
			c.Committer = id
		case "banyan-projected-from":
			c.ProjectedFrom = OID(val)
		case "banyan-synthetic":
			c.Synthetic = val == "true"
		default:
			// 未知头（含 gpgsig 等）忽略——骨架阶段不校验签名体。
		}
	}
	if c.Anchor == "" {
		c.Anchor = RootAnchor
	}
	return c, nil
}

func parseIdentity(s string) (Identity, error) {
	id := Identity{}
	lt, rest, ok := strings.Cut(s, "<")
	if !ok {
		return id, fmt.Errorf("malformed identity %q", s)
	}
	id.Name = strings.TrimSpace(lt)
	email, tail, ok := strings.Cut(rest, ">")
	if !ok {
		return id, fmt.Errorf("malformed identity %q", s)
	}
	id.Email = email
	fields := strings.Fields(tail)
	if len(fields) >= 1 {
		unix, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return id, fmt.Errorf("malformed timestamp in %q", s)
		}
		id.When = time.Unix(unix, 0)
	}
	if len(fields) >= 2 {
		off, err := parseTZOffset(fields[1])
		if err == nil {
			id.When = id.When.In(time.FixedZone("", off))
		}
	}
	return id, nil
}

func parseTZOffset(s string) (int, error) {
	if len(s) != 5 || (s[0] != '+' && s[0] != '-') {
		return 0, fmt.Errorf("malformed tz %q", s)
	}
	h, err1 := strconv.Atoi(s[1:3])
	m, err2 := strconv.Atoi(s[3:5])
	if err1 != nil || err2 != nil {
		return 0, fmt.Errorf("malformed tz %q", s)
	}
	off := (h*3600 + m*60)
	if s[0] == '-' {
		off = -off
	}
	return off, nil
}
