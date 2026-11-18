package oce

import (
	"archive/tar"
	"bytes"
	"context"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// The runtime image OCE's Kubernetes Compute runs for the Agent's gateway:
// OCE's published runtime (RuntimeImage) plus one layer with the substrate
// OpenClaw plugin and kubectl-ate, copied from the always-on-agent gateway
// image (PluginSourceImage). The plugin is compiled JavaScript with vendored
// pure-JavaScript dependencies, and kubectl-ate a static linux/amd64 binary,
// so the layer is made without running either image: no Docker and no
// emulation on an arm64 workstation.
const (
	// RuntimeImage is ghcr.io/openclaw/openclaw-enterprise-runtime:stable on
	// 2026-11-18, source revision 54bfa7fc1b99 (ChartRevision).
	RuntimeImage = "ghcr.io/openclaw/openclaw-enterprise-runtime@sha256:6d0af87f6b2b803975cba47305bf69cd59aef69427290f0490da31f488855cb9"
	// PluginSourceImage is the openclaw plugin's pinned gateway image.
	PluginSourceImage = "ghcr.io/csantanapr/always-on-agent/openclaw-gateway:eks-5bf95fa9017b@sha256:874ed6d8430641df22da07ea9a4e06a5d7b5fc8c34039416fec5f9b3c134326b"
	// PluginDir is where the layer puts the substrate plugin; the Agent's
	// Configuration names it in plugins.load.paths.
	PluginDir = "/opt/oce-substrate/substrate"
	// BridgeDir is where the layer puts the oce-substrate-bridge plugin
	// (bridge/, embedded), which answers each Agent turn from the
	// conversation's actor using the substrate plugin's modules in PluginDir.
	BridgeDir = "/opt/oce-substrate/oce-bridge"
	// KubectlAte is where the layer puts kubectl-ate (the plugin's
	// kubectlAtePath).
	KubectlAte = "/usr/local/bin/kubectl-ate"
	// Arch is the one architecture of the always-on-agent images.
	Arch = "amd64"
)

//go:embed bridge
var bridgeFS embed.FS

// addBridge writes the embedded bridge plugin under BridgeDir, owned by
// 1000:1000, and returns the number of files written.
func addBridge(tw *tar.Writer, epoch time.Time) (int, error) {
	n := 0
	err := fs.WalkDir(bridgeFS, "bridge", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		to := strings.TrimPrefix(BridgeDir, "/") + strings.TrimPrefix(p, "bridge")
		if d.IsDir() {
			return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: to + "/", Mode: 0o755, Uid: 1000, Gid: 1000, ModTime: epoch})
		}
		b, err := bridgeFS.ReadFile(p)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: to, Mode: 0o644, Size: int64(len(b)), Uid: 1000, Gid: 1000, ModTime: epoch}); err != nil {
			return err
		}
		_, err = tw.Write(b)
		n++
		return err
	})
	return n, err
}

// copied maps a path in PluginSourceImage (no leading slash) to its path in
// the layer: a prefix ending in / copies the tree under it.
var copied = map[string]string{
	"app/extensions/substrate/": strings.TrimPrefix(PluginDir, "/") + "/",
	"usr/local/bin/kubectl-ate": strings.TrimPrefix(KubectlAte, "/"),
}

// dest is the layer path of src, or "" when src is not copied.
func dest(src string) string {
	src = strings.TrimPrefix(src, "./")
	for from, to := range copied {
		if strings.HasSuffix(from, "/") {
			if rest, ok := strings.CutPrefix(src, from); ok {
				return to + rest
			}
			if src+"/" == from {
				return strings.TrimSuffix(to, "/")
			}
		} else if src == from {
			return to
		}
	}
	return ""
}

// pluginLayer reads the flattened filesystem of src (a tar stream, as
// mutate.Extract gives it) and returns a layer with the copied paths, owned
// by 1000:1000 (the runtime's node user) and dated at the epoch so the
// layer's digest depends only on the copied bytes.
func pluginLayer(src io.Reader) (v1.Layer, int, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	epoch := time.Unix(0, 0)
	// Parents of the copied trees, so the layer is a valid filesystem.
	for _, d := range []string{"opt/", "opt/oce-substrate/", "usr/", "usr/local/", "usr/local/bin/"} {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: d, Mode: 0o755, ModTime: epoch}); err != nil {
			return nil, 0, err
		}
	}
	tr := tar.NewReader(src)
	n := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, 0, fmt.Errorf("read %s: %w", PluginSourceImage, err)
		}
		to := dest(h.Name)
		if to == "" {
			continue
		}
		out := &tar.Header{Name: to, Mode: h.Mode, Uid: 1000, Gid: 1000, ModTime: epoch}
		switch h.Typeflag {
		case tar.TypeDir:
			out.Typeflag, out.Name = tar.TypeDir, strings.TrimSuffix(to, "/")+"/"
		case tar.TypeReg:
			out.Typeflag, out.Size = tar.TypeReg, h.Size
		case tar.TypeSymlink:
			out.Typeflag, out.Linkname = tar.TypeSymlink, h.Linkname
		default:
			continue
		}
		if err := tw.WriteHeader(out); err != nil {
			return nil, 0, err
		}
		if out.Typeflag == tar.TypeReg {
			if _, err := io.Copy(tw, tr); err != nil {
				return nil, 0, err
			}
			n++
		}
	}
	added, err := addBridge(tw, epoch)
	if err != nil {
		return nil, 0, err
	}
	n += added
	if err := tw.Close(); err != nil {
		return nil, 0, err
	}
	b := buf.Bytes()
	l, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil })
	return l, n, err
}

func pull(ctx context.Context, ref string) (v1.Image, error) {
	r, err := name.ParseReference(ref)
	if err != nil {
		return nil, err
	}
	img, err := remote.Image(r, remote.WithContext(ctx), remote.WithAuth(authn.Anonymous),
		remote.WithPlatform(v1.Platform{OS: "linux", Architecture: Arch}))
	if err != nil {
		return nil, fmt.Errorf("pull %s: %w", ref, err)
	}
	return img, nil
}

// BuildRuntime builds the runtime image and pushes it to repo:tag in the
// caller's private ECR registry, returning repo:tag@digest.
var BuildRuntime = func(ctx context.Context, w io.Writer, ac aws.Config, repo, tag string) (string, error) {
	src, err := pull(ctx, PluginSourceImage)
	if err != nil {
		return "", err
	}
	rc := mutate.Extract(src)
	layer, files, err := pluginLayer(rc)
	rc.Close()
	if err != nil {
		return "", err
	}
	if files == 0 {
		return "", fmt.Errorf("%s has no substrate plugin under /app/extensions/substrate", PluginSourceImage)
	}
	fmt.Fprintf(w, "  substrate plugin and kubectl-ate: %d files from %s\n", files, PluginSourceImage)
	base, err := pull(ctx, RuntimeImage)
	if err != nil {
		return "", err
	}
	img, err := mutate.AppendLayers(base, layer)
	if err != nil {
		return "", err
	}
	host, auth, err := ecrAuth(ctx, ac)
	if err != nil {
		return "", err
	}
	dst, err := name.NewTag(host + "/" + repo + ":" + tag)
	if err != nil {
		return "", err
	}
	if err := remote.Write(dst, img, remote.WithContext(ctx), remote.WithAuth(auth)); err != nil {
		return "", fmt.Errorf("push %s: %w", dst, err)
	}
	d, err := img.Digest()
	if err != nil {
		return "", err
	}
	return dst.String() + "@" + d.String(), nil
}

// ecrAuth is the caller's private registry host and push credential, from
// ecr:GetAuthorizationToken. The credential stays in memory.
func ecrAuth(ctx context.Context, ac aws.Config) (string, authn.Authenticator, error) {
	out, err := ecr.NewFromConfig(ac).GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{})
	if err != nil {
		return "", nil, fmt.Errorf("ecr:GetAuthorizationToken: %w", err)
	}
	if len(out.AuthorizationData) == 0 {
		return "", nil, errors.New("ecr:GetAuthorizationToken returned no authorization data")
	}
	d := out.AuthorizationData[0]
	raw, err := base64.StdEncoding.DecodeString(aws.ToString(d.AuthorizationToken))
	if err != nil {
		return "", nil, errors.New("ecr:GetAuthorizationToken returned a token that is not base64")
	}
	user, pass, ok := strings.Cut(string(raw), ":")
	if !ok {
		return "", nil, errors.New("ecr:GetAuthorizationToken returned a token that is not user:password")
	}
	return strings.TrimPrefix(aws.ToString(d.ProxyEndpoint), "https://"), authn.FromConfig(authn.AuthConfig{Username: user, Password: pass}), nil
}
