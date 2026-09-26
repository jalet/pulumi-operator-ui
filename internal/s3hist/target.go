// Package s3hist reads Pulumi's DIY-backend update history from S3. It is only constructed
// when S3 history is enabled, so nothing loads AWS configuration otherwise.
package s3hist

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/jalet/pulumi-operator-ui/internal/store"
)

// Errors for Stacks and files this package does not read.
var (
	ErrNotS3     = errors.New("backend is not s3://")
	ErrEndpoint  = errors.New("custom S3 endpoints are not supported")
	ErrBadKind   = errors.New("unsupported history kind")
	ErrBadResult = errors.New("unsupported history result")
	ErrTooLarge  = errors.New("history file too large")
	ErrStackName = errors.New("unsupported spec.stack name")
)

// Target is where one Stack's history lives.
type Target struct {
	Namespace, Stack string // the Kubernetes Stack
	Bucket, Region   string // Region "" means the SDK default
	Prefix           string // "<prefix>/.pulumi/history/<project>/<stack>/", no leading slash
}

// TargetFor derives a Stack's history location from its backend URL, for example
// s3://bucket/pulumi/example?region=eu-north-1.
func TargetFor(s store.S3Stack) (Target, error) {
	u, err := url.Parse(s.BackendURL)
	if err != nil || u.Scheme != "s3" {
		return Target{}, ErrNotS3
	}
	if u.Host == "" {
		return Target{}, fmt.Errorf("%w: empty bucket", ErrNotS3)
	}
	q := u.Query()
	if q.Get("endpoint") != "" {
		return Target{}, ErrEndpoint
	}
	stack, err := stackSegment(s.PulumiStack, s.Project)
	if err != nil {
		return Target{}, err
	}
	prefix := path.Join(strings.Trim(u.Path, "/"), ".pulumi/history", s.Project, stack)
	return Target{Namespace: s.Namespace, Stack: s.Name, Bucket: u.Host,
		Region: q.Get("region"), Prefix: strings.TrimPrefix(prefix, "/") + "/"}, nil
}

// stackSegment returns the stack name as it appears in the history path. DIY backends accept
// "stack", "org/stack" and "org/project/stack"; the project segment must match the project.
func stackSegment(name, project string) (string, error) {
	// The project is one path segment; like every stack segment it may not climb out of the
	// history prefix. The Stack CR is user input, and the IAM policy should not be the only
	// boundary.
	if !safeSegment(project) {
		return "", fmt.Errorf("%w: project %q", ErrStackName, project)
	}
	parts := strings.Split(name, "/")
	for _, p := range parts {
		if !safeSegment(p) {
			return "", fmt.Errorf("%w: %q", ErrStackName, name)
		}
	}
	switch len(parts) {
	case 1, 2:
		return parts[len(parts)-1], nil
	case 3:
		if parts[1] != project {
			return "", fmt.Errorf("%w: %q is not in project %q", ErrStackName, name, project)
		}
		return parts[2], nil
	default:
		return "", fmt.Errorf("%w: %q", ErrStackName, name)
	}
}

// safeSegment reports whether s is one non-empty path segment that stays where it is.
func safeSegment(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.Contains(s, "/")
}
