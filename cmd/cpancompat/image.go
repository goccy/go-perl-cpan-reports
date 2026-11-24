package main

import (
	"archive/tar"
	"bytes"
	"debug/buildinfo"
	"embed"
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

// gperlBuild identifies the gperl of the static build image, by the build
// information of its binary (gperl.json in the image).
type gperlBuild struct {
	Module   string `json:"module"`
	Version  string `json:"version"`
	Revision string `json:"revision,omitempty"`
	Modified bool   `json:"modified,omitempty"`
	// DevReplace is the released go-perl version whose source its builds
	// use (GPERL_DEV_REPLACE), "" when they resolve go-perl themselves.
	DevReplace string `json:"dev_replace,omitempty"`
}

// buildStaticImage builds the image of the static build phase
// (Dockerfile.static) around the given gperl binary, built for linux, and
// the image without a single file the binaries are run in (emptyImage).
// devReplace, when given, is the released go-perl version whose source the
// image's builds use, for a gperl whose `gperl build` cannot resolve
// go-perl itself.
func buildStaticImage(dataDir, gperl, devReplace, tag string) error {
	if gperl == "" {
		return fmt.Errorf("image -static: give the gperl binary (-gperl)")
	}
	info, err := buildinfo.ReadFile(gperl)
	if err != nil {
		return fmt.Errorf("image -static: %w", err)
	}
	b := gperlBuild{Module: info.Main.Path, Version: info.Main.Version, DevReplace: devReplace}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			b.Revision = s.Value
		case "vcs.modified":
			b.Modified = s.Value == "true"
		}
	}
	ctx := filepath.Join(dataDir, "static-image-context")
	if err := writeContext(ctx, "Dockerfile.static", "packages", "mysql.sh", "user.sh", "static.sh", "load.pl"); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(ctx, "gperl.json"), b); err != nil {
		return err
	}
	if err := copyFile(gperl, filepath.Join(ctx, "gperl"), 0o755); err != nil {
		return err
	}
	arch, err := exec.Command("docker", "version", "--format", "{{.Server.Arch}}").Output()
	if err != nil {
		return fmt.Errorf("docker version: %w", err)
	}
	cmd := exec.Command("docker", "build", "-t", tag, "-f", filepath.Join(ctx, "Dockerfile.static"),
		"--build-arg", "GO_ARCH="+strings.TrimSpace(string(arch)), "--build-arg", "DEV_REPLACE="+devReplace, ctx)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}
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
