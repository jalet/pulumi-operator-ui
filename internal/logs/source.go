package logs

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// logBytesMax bounds how much of a pod log one capture reads.
const logBytesMax = 4 << 20

// Capture outcomes that are final rather than worth a retry.
var (
	ErrGone      = errors.New("workspace log gone")
	ErrForbidden = errors.New("pods/log forbidden")
)

// Source streams a workspace pod's pulumi container log with kubelet timestamps.
type Source interface {
	Stream(ctx context.Context, namespace, pod string, since time.Time) (io.ReadCloser, error)
}

type kubeSource struct{ cs kubernetes.Interface }

// NewKubeSource reads logs through the Kubernetes API (get on pods/log).
func NewKubeSource(cs kubernetes.Interface) Source { return kubeSource{cs: cs} }

func (k kubeSource) Stream(ctx context.Context, namespace, pod string,
	since time.Time) (io.ReadCloser, error) {
	st := metav1.NewTime(since)
	rc, err := k.cs.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{
		Container: "pulumi", Timestamps: true, SinceTime: &st}).Stream(ctx)
	if err != nil {
		return nil, classify(err)
	}
	return rc, nil
}

// classify maps API errors to ErrGone and ErrForbidden; anything else stays transient.
func classify(err error) error {
	switch {
	case apierrors.IsNotFound(err), apierrors.IsBadRequest(err):
		return fmt.Errorf("%w: %w", ErrGone, err)
	case apierrors.IsForbidden(err):
		return fmt.Errorf("%w: %w", ErrForbidden, err)
	default:
		return err
	}
}

// engineLines returns the pulumi-logger messages stamped within [from, to], stopping at the
// agent's "<op> completed" line, the first line after to, or bytesMax bytes read.
func engineLines(r io.Reader, from, to time.Time, bytesMax int) ([]string, bool, error) {
	var out []string
	read := 0
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		read += len(line) + 1
		if read > bytesMax {
			return out, true, nil
		}
		stamp, rest, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil || at.Before(from) {
			continue
		}
		if at.After(to) {
			return out, false, nil
		}
		// rest: <agent time>\t<LEVEL>\t<logger>\t<message>[\t<fields>]
		parts := strings.SplitN(rest, "\t", 4)
		if len(parts) < 4 {
			continue
		}
		switch parts[2] {
		case "pulumi":
			out = append(out, parts[3])
		case "server":
			if msg, _, _ := strings.Cut(parts[3], "\t"); strings.HasSuffix(msg, " completed") {
				return out, false, nil
			}
		}
	}
	if err := sc.Err(); err != nil {
		return out, false, fmt.Errorf("read log: %w", err)
	}
	return out, false, nil
}
