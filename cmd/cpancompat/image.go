package main

import (
	"archive/tar"
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// imageFiles is the build context of the container image every
// distribution is tested in, minus the gperl binary.
//
//go:embed image
var imageFiles embed.FS

// buildImage builds the image from the embedded context, around either
// the given gperl binary (built for the architecture and operating system
// of the containers, i.e. linux) or, for the reference image, the given
// official perl image (Dockerfile.reference): the same environment with
// perl itself as the perl command. sdk, when given, is the directory of
// the XS SDK headers of a gperl that has no `gperl xs build` to unpack them.
func buildImage(dataDir, gperl, sdk, perlImage, tag string) error {
	if (gperl == "") == (perlImage == "") {
		return fmt.Errorf("image: give either a gperl binary (-gperl) or a perl image (-perl-image)")
	}
	ctx := filepath.Join(dataDir, "image-context")
	entries, err := imageFiles.ReadDir("image")
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if err := writeContext(ctx, names...); err != nil {
		return err
	}
	args := []string{"build", "-t", tag}
	if perlImage != "" {
		args = append(args, "-f", filepath.Join(ctx, "Dockerfile.reference"), "--build-arg", "PERL_IMAGE="+perlImage)
	} else if err := copyFile(gperl, filepath.Join(ctx, "gperl"), 0o755); err != nil {
		return err
	} else if sdk == "" {
		if err := os.Mkdir(filepath.Join(ctx, "sdk"), 0o755); err != nil {
			return err
		}
	} else if err := os.CopyFS(filepath.Join(ctx, "sdk"), os.DirFS(sdk)); err != nil {
		return err
	}
	cmd := exec.Command("docker", append(args, ctx)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// gperlBuild is the build information of the static build image's gperl
// (gperl.json in the image, `go version -m -json` of it).
type gperlBuild struct {
	Path string `json:"Path"`
	Main struct {
		Path    string `json:"Path"`
		Version string `json:"Version"`
		Sum     string `json:"Sum"`
	} `json:"Main"`
	GoVersion string `json:"GoVersion"`
}

// gperlModule is the module gperl is installed from.
const gperlModule = "github.com/goccy/go-perl"

// buildStaticImage builds the image of the static build phase
// (Dockerfile.static), with gperl installed from its module at version,
// and the image without a single file the binaries are run in
// (emptyImage).
func buildStaticImage(dataDir, version, tag string) error {
	if version == "" {
		return fmt.Errorf("image -static: give the gperl version to install (-gperl-version)")
	}
	ctx := filepath.Join(dataDir, "static-image-context")
	if err := writeContext(ctx, "Dockerfile.static", "packages", "mysql.sh", "user.sh", "static.sh", "load.pl"); err != nil {
		return err
	}
	arch, err := exec.Command("docker", "version", "--format", "{{.Server.Arch}}").Output()
	if err != nil {
		return fmt.Errorf("docker version: %w", err)
	}
	cmd := exec.Command("docker", "build", "-t", tag, "-f", filepath.Join(ctx, "Dockerfile.static"),
		"--build-arg", "GO_ARCH="+strings.TrimSpace(string(arch)), "--build-arg", "GPERL_VERSION="+version, ctx)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}
	var stderr strings.Builder
	read := exec.Command("docker", "run", "--rm", "--network", "none", tag, "cat", "/opt/cpancompat/gperl.json")
	read.Stderr = &stderr
	raw, err := read.Output()
	if err != nil {
		return fmt.Errorf("reading the image's gperl.json: %v: %s", err, stderr.String())
	}
	var b gperlBuild
	if err := json.Unmarshal(raw, &b); err != nil {
		return fmt.Errorf("the image's gperl.json: %w", err)
	}
	if b.Main.Path != gperlModule || b.Main.Version != version {
		return fmt.Errorf("the image's gperl is %s %s, not %s %s", b.Main.Path, b.Main.Version, gperlModule, version)
	}
	fmt.Fprintf(os.Stderr, "cpancompat: the image's gperl is %s %s (%s), built with %s\n", b.Main.Path, b.Main.Version, b.Main.Sum, b.GoVersion)
	// An image from an empty archive: no file at all.
	var empty bytes.Buffer
	if err := tar.NewWriter(&empty).Close(); err != nil {
		return err
	}
	imp := exec.Command("docker", "import", "-", emptyImage)
	imp.Stdin = &empty
	if out, err := imp.CombinedOutput(); err != nil {
		return fmt.Errorf("docker import: %v: %s", err, out)
	}
	return nil
}

// writeContext makes dir a build context holding the named files of the
// embedded image directory.
func writeContext(dir string, names ...string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, name := range names {
		data, err := imageFiles.ReadFile("image/" + name)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(from, to string, mode os.FileMode) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
