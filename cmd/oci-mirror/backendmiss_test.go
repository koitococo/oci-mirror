package main

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestBackendTypedMissDistributionDetails(t *testing.T) {
	const repo = "docker.io/library/alpine"
	const digest = "sha256:49340b346798875b5592e5d420241f2f9cfba5f47e62ebb1446a1b34734e898f"
	for _, tc := range []struct {
		name, kind, ref, code, detail string
		want                          backendMissKind
	}{
		{"v3 tag", "manifests", "3.22", "MANIFEST_UNKNOWN", `"unknown tag=3.22"`, exactBackendMiss},
		{"v3 revision", "manifests", digest, "MANIFEST_UNKNOWN", `"unknown manifest name=` + repo + ` revision=` + digest + `"`, exactBackendMiss},
		{"v3 repository", "manifests", "3.22", "NAME_UNKNOWN", `"unknown repository name=` + repo + `"`, exactBackendMiss},
		{"v3 blob", "blobs", digest, "BLOB_UNKNOWN", `"` + digest + `"`, exactBackendMiss},
		{"v2 tag", "manifests", "3.22", "MANIFEST_UNKNOWN", `{"Tag":"3.22"}`, exactBackendMiss},
		{"zot repository", "manifests", "3.22", "NAME_UNKNOWN", `{"name":"` + repo + `"}`, zotNameMiss},
		{"different tag", "manifests", "3.22", "MANIFEST_UNKNOWN", `"unknown tag=3.21"`, notBackendMiss},
		{"wrong repository", "manifests", digest, "MANIFEST_UNKNOWN", `"unknown manifest name=alpine revision=` + digest + `"`, notBackendMiss},
		{"revision is not a tag miss", "manifests", "3.22", "MANIFEST_UNKNOWN", `"unknown manifest name=` + repo + ` revision=` + digest + `"`, notBackendMiss},
		{"unknown tag with extra data", "manifests", "3.22", "MANIFEST_UNKNOWN", `"unknown tag=3.22: invalid"`, notBackendMiss},
		{"media type negotiation", "manifests", "3.22", "MANIFEST_UNKNOWN", `"OCI index found, but accept header does not support OCI indexes"`, notBackendMiss},
		{"denial", "manifests", "3.22", "DENIED", `"unknown tag=3.22"`, notBackendMiss},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"errors":[{"code":%q,"detail":%s}]}`, tc.code, tc.detail)
			resp := &http.Response{Body: io.NopCloser(strings.NewReader(body))}
			read := readRequest{kind: tc.kind, reference: tc.ref, repository: resolvedRepository{downstream: repo}}
			if got := backendTypedMiss(resp, read); got != tc.want {
				t.Fatalf("classified as %d, want %d", got, tc.want)
			}
		})
	}
}
