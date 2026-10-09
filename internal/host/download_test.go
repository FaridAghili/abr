package host

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

type downloadTransport func(*http.Request) (*http.Response, error)

func (f downloadTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFetchGitHubAuthentication(t *testing.T) {
	for _, token := range []string{"", "fixture-token"} {
		for _, destination := range []string{
			"https://api.github.com/repos/example/release",
			"https://github.com/example/release",
			"https://assets.api.github.com/release",
			"https://api.github.com.example.invalid/release",
			"https://api.github.com:8443/release",
		} {
			t.Run(token+"/"+destination, func(t *testing.T) {
				t.Setenv("GITHUB_TOKEN", token)
				original := http.DefaultTransport
				t.Cleanup(func() { http.DefaultTransport = original })
				requests := 0
				http.DefaultTransport = downloadTransport(func(r *http.Request) (*http.Response, error) {
					requests++
					want := ""
					if token != "" && r.URL.Host == "api.github.com" {
						want = "Bearer " + token
					}
					if got := r.Header.Get("Authorization"); got != want {
						t.Fatalf("credential sent incorrectly to %s", r.URL.Host)
					}
					response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("release")), Request: r}
					if requests == 1 {
						response.StatusCode = http.StatusFound
						response.Header.Set("Location", destination)
					}
					return response, nil
				})
				data, err := fetch("https://api.github.com/repos/example/releases/latest", 100)
				if err != nil || string(data) != "release" || requests != 2 {
					t.Fatalf("fetch: data=%q requests=%d err=%v", data, requests, err)
				}
				requests = 1 // Exercise an initial request to each download host too.
				if _, err := fetch(destination, 100); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
