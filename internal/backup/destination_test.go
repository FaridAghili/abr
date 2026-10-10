package backup

import "testing"

func TestDestinationValidation(t *testing.T) {
	d := Destination{Host: "192.0.2.10", User: "backup", Directory: "/srv/backups/abr", Port: 22, Auth: "key"}
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Destination){
		func(d *Destination) { d.Host = "-oProxyCommand=bad" },
		func(d *Destination) { d.User = "root; bad" },
		func(d *Destination) { d.Directory = "/" },
		func(d *Destination) { d.Directory = "/srv/../backup" },
		func(d *Destination) { d.Port = 65536 },
		func(d *Destination) { d.Auth = "password" },
		func(d *Destination) { d.Password = "unexpected" },
	} {
		invalid := d
		change(&invalid)
		if err := invalid.Validate(); err == nil {
			t.Fatal("invalid destination accepted")
		}
	}
	d.Host = "2001:db8::10"
	d.Auth, d.Password = "password", "private fixture password"
	if err := d.Validate(); err != nil {
		t.Fatal("valid IPv6/password destination refused", err)
	}
}
