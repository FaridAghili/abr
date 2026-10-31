package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestGroupCommands(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "config.toml")
	paths := []string{"--config", config, "--state-dir", filepath.Join(dir, "state"), "--apps-dir", filepath.Join(dir, "apps"), "--templates-dir", "../../templates"}
	for _, name := range []string{"api", "shop", "admin"} {
		if _, err := invoke(t, append(paths, "register", "--config-only", "--name", name, "--type", "nuxt", "--domain", name+".test")...); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"group"},
		{"group", "create", "store"},
		{"group", "rename", "store"},
		{"group", "delete"},
		{"group", "list", "extra"},
		{"deploy", "api", "--group", "store"},
		{"restart", "api", "--group", "store"},
		{"status", "api", "--group", "store"},
	} {
		if _, err := invoke(t, append(append(paths, "--dry-run"), args...)...); err == nil {
			t.Fatal("invalid group arguments accepted", args)
		}
	}
	before, _ := os.ReadFile(config)
	if out, err := invoke(t, append(paths, "--dry-run", "group", "create", "store", "shop", "api")...); err != nil || !strings.Contains(out, "Would create group store: shop → api") {
		t.Fatalf("preview inaccurate: %s %v", out, err)
	}
	if after, _ := os.ReadFile(config); string(after) != string(before) {
		t.Fatal("preview changed the configuration")
	}
	for _, args := range [][]string{
		{"group", "create", "store", "shop", "api"},
		{"group", "apps", "store", "api", "shop"},
		{"group", "assign", "admin", "store"},
		{"group", "rename", "store", "shopfront"},
	} {
		if _, err := invoke(t, append(paths, args...)...); err != nil {
			t.Fatal(args, err)
		}
	}
	out, err := invoke(t, append(paths, "group", "list")...)
	if err != nil || !strings.Contains(out, "api → shop → admin") || !strings.Contains(out, "shopfront") {
		t.Fatalf("group list inaccurate: %s %v", out, err)
	}
	out, err = invoke(t, append(paths, "--dry-run", "deploy", "--group", "shopfront")...)
	if err != nil || !regexp.MustCompile(`(?s)api +previewed.*shop +previewed.*admin +previewed`).MatchString(out) {
		t.Fatalf("group deploy preview inaccurate: %s %v", out, err)
	}
	if out, err := invoke(t, append(paths, "group", "delete", "shopfront")...); err != nil || !strings.Contains(out, "its apps are kept") {
		t.Fatal(out, err)
	}
	if out, err := invoke(t, append(paths, "list")...); err != nil || !strings.Contains(out, "admin") {
		t.Fatal("deleting a group removed apps", out, err)
	}
}
