package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestGroupsValidateAndRoundTrip(t *testing.T) {
	c := Default()
	for _, name := range []string{"api", "shop"} {
		a := testApp()
		a.Name, a.User, a.Directory, a.Domain = name, name, "/srv/"+name, name+".example.com"
		c.Apps = append(c.Apps, a)
	}
	c.Groups = []Group{{Name: "store", Apps: []string{"shop", "api"}}, {Name: "empty"}}
	data, err := Encode(c)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(data)
	if err != nil || !reflect.DeepEqual(parsed.Groups[0], c.Groups[0]) || parsed.Groups[1].Name != "empty" {
		t.Fatalf("groups not preserved in order: %+v %v", parsed.Groups, err)
	}
	if g, err := parsed.Group("store"); err != nil || g.Apps[0] != "shop" {
		t.Fatal("group lookup failed", err)
	}
	if _, err := parsed.Group("missing"); err == nil {
		t.Fatal("unknown group found")
	}
	if groups := parsed.GroupsOf("api"); !reflect.DeepEqual(groups, []string{"store"}) {
		t.Fatal("app groups wrong", groups)
	}
	for _, tc := range []struct {
		groups []Group
		err    string
	}{
		{[]Group{{Name: "Store"}}, "name must start"},
		{[]Group{{Name: "store"}, {Name: "store"}}, "duplicate group"},
		{[]Group{{Name: "store", Apps: []string{"missing"}}}, "unknown application"},
		{[]Group{{Name: "store", Apps: []string{"api", "api"}}}, "more than once"},
	} {
		bad := c
		bad.Groups = tc.groups
		if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("%+v: got %v, want %q", tc.groups, err, tc.err)
		}
	}
}
