package plugin

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func (p *fakePlugin) API(_ context.Context, req APIRequest) (APIResponse, error) {
	if req.Path != "echo" {
		return APIResponse{}, ErrNotFound
	}
	return APIResponse{Body: []byte(`{"key":"` + p.config["key"].(string) + `","method":"` + req.Method + `"}`)}, nil
}

func TestPageAPIRequiresEnabledPluginAndRedactsSecrets(t *testing.T) {
	r, _, _ := newFakeRegistry(t)
	ctx := context.Background()
	if _, err := r.API(ctx, "sample", APIRequest{Method: "GET", Path: "echo"}); !errors.Is(err, ErrNotFound) {
		t.Fatal("disabled plugin must not serve its page API")
	}
	if _, err := r.Update("sample", enabledUpdate()); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"../echo", "/echo", "echo/../x", ""} {
		if _, err := r.API(ctx, "sample", APIRequest{Method: "GET", Path: bad}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("path %q must be refused", bad)
		}
	}
	out, err := r.API(ctx, "sample", APIRequest{Method: "POST", Path: "echo"})
	if err != nil || out.Status != 200 || out.ContentType != "application/json" {
		t.Fatal(out, err)
	}
	if strings.Contains(string(out.Body), "initial-value") || !strings.Contains(string(out.Body), "[redacted]") {
		t.Fatalf("secret leaked: %s", out.Body)
	}
}
