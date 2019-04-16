package build

import (
	"bufio"
	"context"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/containerd/containerd/platforms"
	"github.com/docker/distribution/reference"
	"github.com/docker/docker/pkg/urlutil"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/upload/uploadprovider"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/tonistiigi/buildx/driver"
	"github.com/tonistiigi/buildx/util/progress"
	"golang.org/x/sync/errgroup"
)

var (
	errStdinConflict      = errors.New("invalid argument: can't use stdin for both build context and dockerfile")
	errDockerfileConflict = errors.New("ambiguous Dockerfile source: both stdin and flag correspond to Dockerfiles")
)

type Options struct {
	Inputs      Inputs
	Tags        []string
	Labels      map[string]string
	BuildArgs   map[string]string
	Pull        bool
	ImageIDFile string

	NoCache   bool
	Target    string
	Platforms []specs.Platform
	Exports   []client.ExportEntry
	Session   []session.Attachable

	// DockerTarget
}

type Inputs struct {
	ContextPath    string
	DockerfilePath string
	InStream       io.Reader
}

type DriverInfo struct {
	Driver   driver.Driver
	Name     string
	Platform []string // TODO: specs.Platform
	Err      error
}

func getFirstDriver(drivers []DriverInfo) (driver.Driver, error) {
	err := errors.Errorf("no drivers found")
	for _, di := range drivers {
		if di.Driver != nil {
			return di.Driver, nil
		}
		if di.Err != nil {
			err = di.Err
		}
	}
	return nil, err
}

func Build(ctx context.Context, drivers []DriverInfo, opt map[string]Options, pw progress.Writer) (map[string]*client.SolveResponse, error) {
	if len(drivers) == 0 {
		return nil, errors.Errorf("driver required for build")
	}

	if len(drivers) > 1 {
		return nil, errors.Errorf("multiple drivers currently not supported")
	}

	pwOld := pw
	d, err := getFirstDriver(drivers)
	if err != nil {
		return nil, err
	}
	_, isDefaultMobyDriver := d.(interface {
		IsDefaultMobyDriver()
	})
	c, pw, err := driver.Boot(ctx, d, pw)
	if err != nil {
		close(pwOld.Status())
		<-pwOld.Done()
		return nil, err
	}

	withPrefix := len(opt) > 1

	mw := progress.NewMultiWriter(pw)

	eg, ctx := errgroup.WithContext(ctx)

	resp := map[string]*client.SolveResponse{}
	var mu sync.Mutex

	for k, opt := range opt {
		pw := mw.WithPrefix(k, withPrefix)

		if opt.ImageIDFile != "" {
			if len(opt.Platforms) != 0 {
				return nil, errors.Errorf("image ID file cannot be specified when building for multiple platforms")
			}
			// Avoid leaving a stale file if we eventually fail
			if err := os.Remove(opt.ImageIDFile); err != nil && !os.IsNotExist(err) {
				return nil, errors.Wrap(err, "removing image ID file")
			}
		}

		so := client.SolveOpt{
			Frontend:      "dockerfile.v0",
			FrontendAttrs: map[string]string{},
			LocalDirs:     map[string]string{},
		}

		switch len(opt.Exports) {
		case 1:
			// valid
		case 0:
			if isDefaultMobyDriver {
				// backwards compat for docker driver only:
				// this ensures the build results in a docker image.
				opt.Exports = []client.ExportEntry{{Type: "image", Attrs: map[string]string{}}}
			}
		default:
			return nil, errors.Errorf("multiple outputs currently unsupported")
		}

		if len(opt.Tags) > 0 {
			tags := make([]string, len(opt.Tags))
			for i, tag := range opt.Tags {
				ref, err := reference.Parse(tag)
				if err != nil {
					return nil, errors.Wrapf(err, "invalid tag %q", tag)
				}
				tags[i] = ref.String()
			}
			for i, e := range opt.Exports {
				switch e.Type {
				case "image", "oci", "docker":
					opt.Exports[i].Attrs["name"] = strings.Join(tags, ",")
				}
			}
		} else {
			for _, e := range opt.Exports {
				if e.Type == "image" && e.Attrs["name"] == "" && e.Attrs["push"] != "" {
					if ok, _ := strconv.ParseBool(e.Attrs["push"]); ok {
						return nil, errors.Errorf("tag is needed when pushing to registry")
					}
				}
			}
		}

		for i, e := range opt.Exports {
			if (e.Type == "local" || e.Type == "tar") && opt.ImageIDFile != "" {
				return nil, errors.Errorf("local and tar exporters are incompatible with image ID file")
			}
			if e.Type == "oci" && !d.Features()[driver.OCIExporter] {
				return nil, notSupported(d, driver.OCIExporter)
			}
			if e.Type == "docker" {
				if e.Output == nil {
					if !isDefaultMobyDriver {
						return nil, errors.Errorf("loading to docker currently not implemented, specify dest file or -")
					}
					e.Type = "image"
				} else if !d.Features()[driver.DockerExporter] {
					return nil, notSupported(d, driver.DockerExporter)
				}
			}
			if e.Type == "image" && isDefaultMobyDriver {
				opt.Exports[i].Type = "moby"
				if e.Attrs["push"] != "" {
					if ok, _ := strconv.ParseBool(e.Attrs["push"]); ok {
						return nil, errors.Errorf("auto-push is currently not implemented for docker driver")
					}
				}
			}
		}

		// TODO: handle loading to docker daemon

		so.Exports = opt.Exports
		so.Session = opt.Session

		release, err := LoadInputs(opt.Inputs, &so)
		if err != nil {
			return nil, err
		}
		defer release()

		if opt.Pull {
			so.FrontendAttrs["image-resolve-mode"] = "pull"
		}
		if opt.Target != "" {
			so.FrontendAttrs["target"] = opt.Target
		}
		if opt.NoCache {
			so.FrontendAttrs["no-cache"] = ""
		}
		for k, v := range opt.BuildArgs {
			so.FrontendAttrs["build-arg:"+k] = v
		}
		for k, v := range opt.Labels {
			so.FrontendAttrs["label:"+k] = v
		}

		if len(opt.Platforms) != 0 {
			pp := make([]string, len(opt.Platforms))
			for i, p := range opt.Platforms {
				pp[i] = platforms.Format(p)
			}
			if len(pp) > 1 && !d.Features()[driver.MultiPlatform] {
				return nil, notSupported(d, driver.MultiPlatform)
			}
			so.FrontendAttrs["platform"] = strings.Join(pp, ",")
		}

		var statusCh chan *client.SolveStatus
		if pw != nil {
			statusCh = pw.Status()
			eg.Go(func() error {
				<-pw.Done()
				return pw.Err()
			})
		}

		eg.Go(func() error {
			rr, err := c.Solve(ctx, nil, so, statusCh)
			if err != nil {
				return err
			}
			mu.Lock()
			resp[k] = rr
			mu.Unlock()
			if opt.ImageIDFile != "" {
				return ioutil.WriteFile(opt.ImageIDFile, []byte(rr.ExporterResponse["containerimage.digest"]), 0644)
			}
			return nil
		})
	}

	if err := eg.Wait(); err != nil {
		return nil, err
	}

	return resp, nil
}

func createTempDockerfile(r io.Reader) (string, error) {
	dir, err := ioutil.TempDir("", "dockerfile")
	if err != nil {
		return "", err
	}
	f, err := os.Create(filepath.Join(dir, "Dockerfile"))
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(f, r); err != nil {
		return "", err
	}
	return dir, err
}

func LoadInputs(inp Inputs, target *client.SolveOpt) (func(), error) {
	if inp.ContextPath == "" {
		return nil, errors.New("please specify build context (e.g. \".\" for the current directory)")
	}

	// TODO: handle stdin, symlinks, remote contexts, check files exist

	var (
		err              error
		dockerfileReader io.Reader
		dockerfileDir    string
		dockerfileName   = inp.DockerfilePath
		toRemove         []string
	)

	switch {
	case inp.ContextPath == "-":
		if inp.DockerfilePath == "-" {
			return nil, errStdinConflict
		}

		buf := bufio.NewReader(os.Stdin)
		magic, err := buf.Peek(archiveHeaderSize * 2)
		if err != nil && err != io.EOF {
			return nil, errors.Wrap(err, "failed to peek context header from STDIN")
		}

		if isArchive(magic) {
			// stdin is context
			up := uploadprovider.New()
			target.FrontendAttrs["context"] = up.Add(buf)
			target.Session = append(target.Session, up)
		} else {
			if inp.DockerfilePath != "" {
				return nil, errDockerfileConflict
			}
			// stdin is dockerfile
			dockerfileReader = buf
			inp.ContextPath, _ = ioutil.TempDir("", "empty-dir")
			toRemove = append(toRemove, inp.ContextPath)
			target.LocalDirs["context"] = inp.ContextPath
		}

	case isLocalDir(inp.ContextPath):
		target.LocalDirs["context"] = inp.ContextPath
		switch inp.DockerfilePath {
		case "-":
			dockerfileReader = os.Stdin
		case "":
			dockerfileDir = inp.ContextPath
		default:
			dockerfileDir = filepath.Dir(inp.DockerfilePath)
			dockerfileName = filepath.Base(inp.DockerfilePath)
		}

	case urlutil.IsGitURL(inp.ContextPath), urlutil.IsURL(inp.ContextPath):
		if inp.DockerfilePath == "-" {
			return nil, errors.Errorf("Dockerfile from stdin is not supported with remote contexts")
		}
		target.FrontendAttrs["context"] = inp.ContextPath
	default:
		return nil, errors.Errorf("unable to prepare context: path %q not found", inp.ContextPath)
	}

	if dockerfileReader != nil {
		dockerfileDir, err = createTempDockerfile(dockerfileReader)
		if err != nil {
			return nil, err
		}
		toRemove = append(toRemove, dockerfileDir)
	}

	if dockerfileName == "" {
		dockerfileName = "Dockerfile"
	}
	target.FrontendAttrs["filename"] = dockerfileName

	if dockerfileDir != "" {
		target.LocalDirs["dockerfile"] = dockerfileDir
	}

	release := func() {
		for _, dir := range toRemove {
			os.RemoveAll(dir)
		}
	}
	return release, nil
}

func notSupported(d driver.Driver, f driver.Feature) error {
	return errors.Errorf("%s feature is currently not supported for %s driver. Please switch to a different driver (eg. \"docker buildx new\")", f, d.Factory().Name())
}
