package gitsync

import (
	"net/http"
	"net/url"
	"testing"
)

func TestFollowRedirectHook_ReturnsRequestURL(t *testing.T) {
	want := &url.URL{Scheme: "https", Host: "node.example", Path: "/repo.git/info/refs"}
	res := &http.Response{Request: &http.Request{URL: want}}

	got := FollowRedirectHook(res)
	if got != want {
		t.Errorf("FollowRedirectHook = %v, want %v", got, want)
	}
}

func TestFollowRedirectHook_NilSafe(t *testing.T) {
	if got := FollowRedirectHook(nil); got != nil {
		t.Errorf("FollowRedirectHook(nil) = %v, want nil", got)
	}
	if got := FollowRedirectHook(&http.Response{}); got != nil {
		t.Errorf("FollowRedirectHook(no Request) = %v, want nil", got)
	}
	if got := FollowRedirectHook(&http.Response{Request: &http.Request{}}); got != nil {
		t.Errorf("FollowRedirectHook(no URL) = %v, want nil", got)
	}
}
