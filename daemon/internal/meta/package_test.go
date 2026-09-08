package meta

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestDebFormats(t *testing.T) {
	for _, comp := range []string{"gz", "xz", "zst", ""} {
		t.Run("control.tar."+comp, func(t *testing.T) {
			b := buildDeb(comp, sampleControl)
			m := extractBytes(t, &debExtractor{}, "pkg.deb", b)
			want := map[string]string{
				"package.format": "deb", "package.name": "filebrowserd", "package.version": "1.2.3-1",
				"package.arch": "amd64", "package.section": "utils", "package.maintainer": "Example Maintainer <maint@example.com>",
				"package.summary": "Browse Unraid shares from the webGUI", "package.installedSize": "4194304",
			}
			for k, v := range want {
				if got := m.value(k); got != v {
					t.Errorf("%s = %q, want %q", k, got, v)
				}
			}
			if got := strings.Join(m.values("package.depends"), ","); got != "libc6,libssl3,libssl1.1,ffmpeg,dpkg" {
				t.Errorf("depends = %q", got)
			}
			if m.num("package.installedSize") != 4096*1024 {
				t.Errorf("installedSize num = %v", m.num("package.installedSize"))
			}
		})
	}
}

func TestDebRejects(t *testing.T) {
	ex := &debExtractor{}
	bad := map[string][]byte{
		"not-ar":     []byte("!<arch>\nnope"),
		"zip":        []byte("PK\x03\x04................"),
		"no-control": arArchive(arMember{"debian-binary", []byte("2.0\n")}, arMember{"data.tar.gz", gzipOf(tarOf(map[string]string{"x": "y"}))}),
		"huge-size":  arArchive(arMember{"control.tar.gz", []byte("x")}),
	}
	// Forge an absurd member size on the last one.
	copy(bad["huge-size"][8+48:], "9999999999")
	for name, b := range bad {
		_, err := Run(context.Background(), ex, name+".deb", bytes.NewReader(b), int64(len(b)))
		if err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	// A control member whose tar has no control file.
	b := arArchive(arMember{"debian-binary", []byte("2.0\n")},
		arMember{"control.tar.gz", gzipOf(tarOf(map[string]string{"./md5sums": "x"}))})
	if _, err := Run(context.Background(), ex, "x.deb", bytes.NewReader(b), int64(len(b))); err == nil {
		t.Error("missing control file: expected error")
	}
}

func TestParseControl(t *testing.T) {
	c := parseControl([]byte("Package: a\nDescription: first\n second\n\nPackage: other\n"))
	if c["package"] != "a" || c["description"] != "first\nsecond" {
		t.Errorf("parseControl = %v", c)
	}
	deps := splitDepends("libc6 (>= 2.34) [amd64], python3:any | python3-minimal,, foo<bar>")
	if strings.Join(deps, ",") != "libc6,python3,python3-minimal,foo" {
		t.Errorf("splitDepends = %v", deps)
	}
}

func TestRPM(t *testing.T) {
	b := buildRPM(sampleRPMTags)
	m := extractBytes(t, &rpmExtractor{}, "htop.rpm", b)
	want := map[string]string{
		"package.format": "rpm", "package.name": "htop", "package.version": "3.3.0", "package.release": "2.fc40",
		"package.summary": "Interactive process viewer", "package.arch": "x86_64", "package.section": "Applications/System",
		"package.maintainer": "Fedora Project <packager@fedoraproject.org>", "package.license": "GPL-2.0-or-later",
		"package.vendor": "Fedora Project", "package.installedSize": "409600",
	}
	for k, v := range want {
		if got := m.value(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if got := strings.Join(m.values("package.depends"), ","); got != "libc.so.6()(64bit),libncursesw.so.6()(64bit)" {
		t.Errorf("depends = %q (rpmlib/file deps must be dropped)", got)
	}
}

func TestRPMRejects(t *testing.T) {
	ex := &rpmExtractor{}
	good := buildRPM(sampleRPMTags)
	cases := map[string]func([]byte) []byte{
		"bad-lead":       func(b []byte) []byte { b[0] = 0; return b },
		"bad-hdr-magic":  func(b []byte) []byte { b[rpmLeadLen] = 0; return b },
		"huge-nindex":    func(b []byte) []byte { copy(b[rpmLeadLen+8:], []byte{0xff, 0xff, 0xff, 0xff}); return b },
		"huge-store":     func(b []byte) []byte { copy(b[rpmLeadLen+12:], []byte{0x7f, 0xff, 0xff, 0xff}); return b },
		"truncated-lead": func(b []byte) []byte { return b[:50] },
		"truncated-hdr":  func(b []byte) []byte { return b[:rpmLeadLen+40] },
	}
	for name, mut := range cases {
		b := mut(append([]byte(nil), good...))
		if _, err := Run(context.Background(), ex, name+".rpm", bytes.NewReader(b), int64(len(b))); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	// An index entry pointing past the store must be rejected, not read.
	b := append([]byte(nil), good...)
	// Main header starts after lead + padded signature header.
	sig, _ := readRPMHeader(bytes.NewReader(b), rpmLeadLen, int64(len(b)))
	next := sig.store + sig.size
	next += (8 - next%8) % 8
	copy(b[next+16+8:], []byte{0x7f, 0xff, 0xff, 0xff}) // first entry offset
	if _, err := Run(context.Background(), ex, "badoff.rpm", bytes.NewReader(b), int64(len(b))); err == nil {
		t.Error("out-of-range offset: expected error")
	}
}
