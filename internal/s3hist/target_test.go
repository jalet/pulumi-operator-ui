package s3hist

import (
	"errors"
	"testing"

	"github.com/jalet/pulumi-operator-ui/internal/store"
)

func stackWith(backend string) store.S3Stack {
	return store.S3Stack{Namespace: "ns", Name: "app", BackendURL: backend, Project: "proj",
		PulumiStack: "dev"}
}

func TestTargetFor(t *testing.T) {
	tests := []struct {
		give                   string
		wantBucket, wantRegion string
		wantPrefix             string
		wantErr                error
	}{
		{give: "s3://b/pulumi/example?region=eu-north-1", wantBucket: "b", wantRegion: "eu-north-1",
			wantPrefix: "pulumi/example/.pulumi/history/proj/dev/"},
		{give: "s3://b/pulumi/example/", wantBucket: "b", wantPrefix: "pulumi/example/.pulumi/history/proj/dev/"},
		{give: "s3://b", wantBucket: "b", wantPrefix: ".pulumi/history/proj/dev/"},
		{give: "s3://b/p?awssdk=v2&region=eu-west-1", wantBucket: "b", wantRegion: "eu-west-1",
			wantPrefix: "p/.pulumi/history/proj/dev/"},
		{give: "s3://b/p?endpoint=minio:9000", wantErr: ErrEndpoint},
		{give: "https://api.pulumi.com", wantErr: ErrNotS3},
		{give: "file:///state", wantErr: ErrNotS3},
		{give: "azblob://c", wantErr: ErrNotS3},
		{give: "s3://", wantErr: ErrNotS3},
	}
	for _, tt := range tests {
		t.Run(tt.give, func(t *testing.T) {
			got, err := TargetFor(stackWith(tt.give))
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := Target{Namespace: "ns", Stack: "app", Bucket: tt.wantBucket,
				Region: tt.wantRegion, Prefix: tt.wantPrefix}
			if got != want {
				t.Fatalf("got %+v, want %+v", got, want)
			}
		})
	}
}

func FuzzTargetFor(f *testing.F) {
	for _, s := range []string{"s3://b/p?region=x", "s3://", "s3:///", "::", "s3://b/%zz"} {
		f.Add(s)
	}
	f.Fuzz(func(_ *testing.T, backend string) { _, _ = TargetFor(stackWith(backend)) })
}

func TestTargetForQualifiedStackNames(t *testing.T) {
	tests := []struct {
		give       string
		wantPrefix string
		wantErr    bool
	}{
		{give: "dev", wantPrefix: "p/.pulumi/history/proj/dev/"},
		{give: "organization/proj/dev", wantPrefix: "p/.pulumi/history/proj/dev/"},
		{give: "organization/dev", wantPrefix: "p/.pulumi/history/proj/dev/"},
		{give: "organization/other/dev", wantErr: true}, // project does not match
		{give: "a/b/c/d", wantErr: true},
		{give: "organization/proj/", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.give, func(t *testing.T) {
			s := stackWith("s3://b/p")
			s.PulumiStack = tt.give
			got, err := TargetFor(s)
			if tt.wantErr {
				if !errors.Is(err, ErrStackName) {
					t.Fatalf("err = %v, want ErrStackName", err)
				}
				return
			}
			if err != nil || got.Prefix != tt.wantPrefix {
				t.Fatalf("got %+v, %v; want prefix %q", got, err, tt.wantPrefix)
			}
		})
	}
}
