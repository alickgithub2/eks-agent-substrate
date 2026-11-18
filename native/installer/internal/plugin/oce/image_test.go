package oce

import (
	"archive/tar"
	"bytes"
	"io"
	"testing"
)

func TestDest(t *testing.T) {
	for src, want := range map[string]string{
		"app/extensions/substrate/dist/index.js":  "opt/oce-substrate/substrate/dist/index.js",
		"./app/extensions/substrate/package.json": "opt/oce-substrate/substrate/package.json",
		"app/extensions/substrate":                "opt/oce-substrate/substrate",
		"usr/local/bin/kubectl-ate":               "usr/local/bin/kubectl-ate",
		"usr/local/bin/node":                      "",
		"app/extensions/substrate-other/x":        "",
		"app/openclaw.mjs":                        "",
	} {
		if got := dest(src); got != want {
			t.Errorf("dest(%q) = %q, want %q", src, got, want)
		}
	}
}

func TestPluginLayer(t *testing.T) {
	var src bytes.Buffer
	tw := tar.NewWriter(&src)
	add := func(name string, typ byte, body string) {
		tw.WriteHeader(&tar.Header{Name: name, Typeflag: typ, Mode: 0o644, Size: int64(len(body)), Uid: 0})
		tw.Write([]byte(body))
	}
	add("app/extensions/substrate/", tar.TypeDir, "")
	add("app/extensions/substrate/dist/index.js", tar.TypeReg, "export default {}")
	add("usr/local/bin/kubectl-ate", tar.TypeReg, "ELF")
	add("etc/passwd", tar.TypeReg, "root")
	tw.Close()
	l, n, err := pluginLayer(&src)
	if err != nil || n < 5 {
		t.Fatalf("pluginLayer = %d files, %v; want the 2 copied and the bridge's", n, err)
	}
	rc, _ := l.Uncompressed()
	tr := tar.NewReader(rc)
	got := map[string]int{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg && (h.Uid != 1000 || h.Gid != 1000) {
			t.Errorf("%s owned by %d:%d, want 1000:1000 (OpenClaw refuses other owners)", h.Name, h.Uid, h.Gid)
		}
		got[h.Name]++
	}
	for _, want := range []string{"opt/oce-substrate/substrate/dist/index.js", "usr/local/bin/kubectl-ate", "opt/oce-substrate/substrate/", "opt/oce-substrate/oce-bridge/dist/index.js", "opt/oce-substrate/oce-bridge/openclaw.plugin.json"} {
		if got[want] == 0 {
			t.Errorf("layer lacks %s: %v", want, got)
		}
	}
	if got["etc/passwd"] != 0 {
		t.Error("layer copied etc/passwd")
	}
}
