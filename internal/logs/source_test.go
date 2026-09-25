package logs

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var _t0 = time.Date(2026, 9, 25, 14, 48, 0, 0, time.UTC)

// kline formats one kubelet-timestamped agent line.
func kline(at time.Time, logger, msg string) string {
	return at.Format(time.RFC3339Nano) + " " + at.Format("2006-01-02T15:04:05.000Z") +
		"\tINFO\t" + logger + "\t" + msg + "\n"
}

func TestEngineLinesWindow(t *testing.T) {
	var b strings.Builder
	b.WriteString(kline(_t0.Add(-time.Minute), "pulumi", "previous run"))
	b.WriteString(kline(_t0, "pulumi", "Updating (prod):"))
	b.WriteString(kline(_t0.Add(time.Second), "server", "selected a stack\t{\"name\": \"prod\"}"))
	b.WriteString(kline(_t0.Add(2*time.Second), "pulumi", "    ~ a:b/c:D: (update)"))
	b.WriteString(kline(_t0.Add(3*time.Second), "server", "up completed\t{\"result\": \"succeeded\"}"))
	b.WriteString(kline(_t0.Add(4*time.Second), "pulumi", "next run"))
	got, truncated, err := engineLines(strings.NewReader(b.String()), _t0.Add(-5*time.Second),
		_t0.Add(time.Hour), logBytesMax)
	if err != nil || truncated {
		t.Fatalf("err=%v truncated=%v", err, truncated)
	}
	want := []string{"Updating (prod):", "    ~ a:b/c:D: (update)"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("(-want +got):\n%s", diff)
	}
}

func TestEngineLinesStopsAtWindowEnd(t *testing.T) {
	var b strings.Builder
	b.WriteString(kline(_t0, "pulumi", "in"))
	b.WriteString(kline(_t0.Add(time.Minute), "pulumi", "after"))
	got, _, err := engineLines(strings.NewReader(b.String()), _t0.Add(-time.Second),
		_t0.Add(time.Second), logBytesMax)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"in"}, got); diff != "" {
		t.Fatalf("(-want +got):\n%s", diff)
	}
}

func TestEngineLinesByteCap(t *testing.T) {
	var b strings.Builder
	for range 100 {
		b.WriteString(kline(_t0, "pulumi", strings.Repeat("x", 100)))
	}
	got, truncated, err := engineLines(strings.NewReader(b.String()), _t0.Add(-time.Second),
		_t0.Add(time.Second), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || len(got) >= 100 {
		t.Fatalf("lines=%d truncated=%v", len(got), truncated)
	}
}

func TestEngineLinesKeepsTabsInMessage(t *testing.T) {
	got, _, err := engineLines(strings.NewReader(kline(_t0, "pulumi", "a\tb")),
		_t0.Add(-time.Second), _t0.Add(time.Second), logBytesMax)
	if err != nil || len(got) != 1 || got[0] != "a\tb" {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestClassify(t *testing.T) {
	gr := schema.GroupResource{Resource: "pods"}
	for _, tt := range []struct {
		give error
		want error
	}{
		{apierrors.NewNotFound(gr, "p"), ErrGone},
		{apierrors.NewBadRequest(`container "pulumi" in pod "p" is not valid`), ErrGone},
		{apierrors.NewForbidden(gr, "p", errors.New("no")), ErrForbidden},
	} {
		if got := classify(tt.give); !errors.Is(got, tt.want) {
			t.Errorf("classify(%v) = %v, want %v", tt.give, got, tt.want)
		}
	}
	other := errors.New("connection reset")
	if got := classify(other); errors.Is(got, ErrGone) || errors.Is(got, ErrForbidden) {
		t.Errorf("transient error classified as %v", got)
	}
}
