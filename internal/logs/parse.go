// Package logs captures a finished run's engine output from the workspace pod log and
// parses the resources it changed.
package logs

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/jalet/pulumi-operator-ui/internal/store"
)

const (
	diffBytesMax = 64 << 10
	runBytesMax  = 1 << 20
	resourcesMax = 2000
)

// Result is what one run's engine output says it changed.
type Result struct {
	Counts    map[string]int64
	Resources []store.LogResource
	Summary   bool // a Resources: summary was found
	Truncated bool // resources or diff bytes were dropped at a cap
}

var (
	// Resource header: indent, optional op symbol, type (always contains ':'), op in parens,
	// optional lock glyph for resources with secrets.
	_header = regexp.MustCompile(`^( *)(\+-|-\+|\+\+|--|[-+~>=])? *(\S+:\S+): ` +
		`\(([a-z-]+)\)(?: \S+)?$`)
	_meta  = regexp.MustCompile(`^\s*\[(urn|id|provider)=(.*)\]$`)
	_count = regexp.MustCompile(`(\d+) (?:to )?(create|created|update|updated|` +
		`delete|deleted|replace|replaced|import|imported|unchanged)\b`)

	_listed = map[string]bool{"create": true, "update": true, "delete": true, "replace": true,
		"create-replacement": true, "delete-replaced": true, "import": true}
	_known = map[string]bool{"same": true, "read": true, "refresh": true, "discard": true}

	_countKeys = map[string]string{"create": "create", "created": "create", "update": "update",
		"updated": "update", "delete": "delete", "deleted": "delete", "replace": "replace",
		"replaced": "replace", "import": "import", "imported": "import", "unchanged": "same"}
)

// block is the resource being read.
type block struct {
	res    store.LogResource
	indent int
	skip   bool // a Stack entry or an op that is never listed
	diff   []string
}

// Parse reads engine messages in order. It never fails: unknown lines are ignored.
func Parse(lines []string) Result {
	res := Result{Counts: map[string]int64{}}
	var cur *block
	inSummary := false
	budget := runBytesMax
	flush := func() {
		if cur == nil {
			return
		}
		b := cur
		cur = nil
		if b.skip || (b.res.Op == "refresh" && len(b.diff) == 0) {
			return
		}
		if len(res.Resources) >= resourcesMax {
			res.Truncated = true
			return
		}
		b.res.Diff, b.res.Truncated = joinDiff(b.diff, min(diffBytesMax, budget))
		budget -= len(b.res.Diff)
		if b.res.Truncated {
			res.Truncated = true
		}
		res.Resources = append(res.Resources, b.res)
	}
	for _, line := range lines {
		if inSummary {
			if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "Duration:") {
				inSummary = false
				continue
			}
			for _, m := range _count.FindAllStringSubmatch(line, -1) {
				n, err := strconv.ParseInt(m[1], 10, 64)
				if err == nil {
					res.Counts[_countKeys[m[2]]] += n
				}
			}
			continue
		}
		if line == "Resources:" {
			flush()
			inSummary, res.Summary = true, true
			continue
		}
		if m := _header.FindStringSubmatch(line); m != nil && (_listed[m[4]] || _known[m[4]]) {
			flush()
			cur = &block{indent: len(m[1]), res: store.LogResource{Op: m[4], Type: m[3]},
				skip: m[3] == "pulumi:pulumi:Stack" || (!_listed[m[4]] && m[4] != "refresh")}
			continue
		}
		if cur == nil {
			continue
		}
		if mm := _meta.FindStringSubmatch(line); mm != nil {
			if mm[1] == "urn" {
				cur.res.URN = mm[2]
				cur.res.Name = mm[2][strings.LastIndex(mm[2], "::")+2:]
			}
			continue
		}
		trimmed := strings.TrimLeft(line, " ")
		if trimmed == "" {
			continue
		}
		if len(line)-len(trimmed) <= cur.indent {
			flush()
			continue
		}
		if trimmed == "--outputs:--" {
			// A refresh prints what drifted as outputs; for other ops the outputs only
			// repeat the inputs (and a Stack's carry values like the account ID).
			if cur.res.Op != "refresh" {
				flush()
			}
			continue
		}
		cur.diff = append(cur.diff, line)
	}
	flush()
	return res
}

// joinDiff removes the common indent and joins lines, cutting at limit bytes.
func joinDiff(lines []string, limit int) (string, bool) {
	common := -1
	for _, l := range lines {
		n := len(l) - len(strings.TrimLeft(l, " "))
		if common < 0 || n < common {
			common = n
		}
	}
	var b strings.Builder
	for i, l := range lines {
		l = l[max(common, 0):]
		if b.Len()+len(l)+1 > limit {
			return b.String(), true
		}
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(l)
	}
	return b.String(), false
}
