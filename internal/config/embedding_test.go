package config

import (
	"reflect"
	"testing"
)

func TestEmbeddingValidationAndRoundTrip(t *testing.T) {
	for _, e := range []Embedding{
		{}, {Paths: []string{"/banner.html", "/ads/*"}, Origins: []string{"*"}},
		{Paths: []string{"/*"}, Origins: []string{"self", "https://partner.example.com", "http://localhost:8080", "https://[::1]:443"}},
	} {
		if err := e.Validate(); err != nil {
			t.Fatalf("%+v: %v", e, err)
		}
	}
	for _, e := range []Embedding{
		{Paths: []string{"/banner.html"}}, {Origins: []string{"*"}},
		{Paths: []string{"banner.html"}, Origins: []string{"*"}},
		{Paths: []string{"/ads*"}, Origins: []string{"*"}},
		{Paths: []string{"/ads/{path}"}, Origins: []string{"*"}},
		{Paths: []string{"/../admin"}, Origins: []string{"*"}},
		{Paths: []string{"/a\nheader"}, Origins: []string{"*"}},
		{Paths: []string{"/a", "/a"}, Origins: []string{"*"}},
		{Paths: []string{"/a"}, Origins: []string{"*", "self"}},
		{Paths: []string{"/a"}, Origins: []string{"https://partner.example.com/path"}},
		{Paths: []string{"/a"}, Origins: []string{"https://user:secret@partner.example.com"}},
		{Paths: []string{"/a"}, Origins: []string{"https://partner.example.com; frame-src *"}},
		{Paths: []string{"/a"}, Origins: []string{"https://{host}"}},
		{Paths: []string{"/a"}, Origins: []string{"https://partner.example.com:99999"}},
		{Paths: []string{"/a"}, Origins: []string{"https://partner.example.com:"}},
		{Paths: []string{"/a"}, Origins: []string{"self", "self"}},
	} {
		if err := e.Validate(); err == nil {
			t.Fatalf("accepted %+v", e)
		}
	}
	c := Default()
	c.Apps = []App{{Name: "banner", Directory: "/srv/apps/banner", User: "abr-banner", Type: "nuxt", Domain: "banner.example.com", Embedding: Embedding{Paths: []string{"/banner.html", "/ads/*"}, Origins: []string{"self", "https://partner.example.com"}}}}
	data, err := Encode(c)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(data)
	if err != nil || !reflect.DeepEqual(got, c) {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	if got.Apps[0].Embedding.FrameAncestors() != "frame-ancestors 'self' https://partner.example.com;" {
		t.Fatal("invalid CSP")
	}
}

func TestEmbeddingEditPreservesAndClears(t *testing.T) {
	a := App{Name: "app", User: "abr-app", Directory: "/srv/apps/app", Type: "nuxt", Domain: "app.example.com", Embedding: Embedding{Paths: []string{"/banner.html"}, Origins: []string{"*"}}}
	got, err := (AppChanges{Values: App{HealthCheck: "https://app.example.com/up"}, Fields: []string{"health-check"}}).Apply(a)
	if err != nil || !reflect.DeepEqual(got.Embedding, a.Embedding) {
		t.Fatal("unrelated edit changed embedding", err)
	}
	got, err = (AppChanges{Values: App{Embedding: Embedding{Origins: []string{"self"}}}, Fields: []string{"embed-origin"}}).Apply(a)
	if err != nil || !reflect.DeepEqual(got.Embedding.Paths, a.Embedding.Paths) || got.Embedding.Origins[0] != "self" {
		t.Fatal("origin edit lost paths", err)
	}
	got, err = (AppChanges{Fields: []string{"embed-path", "embed-origin"}}).Apply(a)
	if err != nil || len(got.Embedding.Paths) != 0 || len(got.Embedding.Origins) != 0 {
		t.Fatal("clear failed", err)
	}
}
