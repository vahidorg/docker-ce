package image

import (
	"bytes"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"syscall"
	"testing"

	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/internal/test"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/pkg/archive"
	"github.com/gotestyourself/gotestyourself/assert"
	is "github.com/gotestyourself/gotestyourself/assert/cmp"
	"github.com/gotestyourself/gotestyourself/fs"
	"github.com/gotestyourself/gotestyourself/skip"
	"golang.org/x/net/context"
)

func TestRunBuildResetsUidAndGidInContext(t *testing.T) {
	skip.IfCondition(t, runtime.GOOS == "windows", "uid and gid not relevant on windows")
	dest := fs.NewDir(t, "test-build-context-dest")
	defer dest.Remove()

	fakeImageBuild := func(_ context.Context, context io.Reader, options types.ImageBuildOptions) (types.ImageBuildResponse, error) {
		assert.Check(t, archive.Untar(context, dest.Path(), nil))

		body := new(bytes.Buffer)
		return types.ImageBuildResponse{Body: ioutil.NopCloser(body)}, nil
	}
	cli := test.NewFakeCli(&fakeClient{imageBuildFunc: fakeImageBuild})

	dir := fs.NewDir(t, "test-build-context",
		fs.WithFile("foo", "some content", fs.AsUser(65534, 65534)),
		fs.WithFile("Dockerfile", `
			FROM alpine:3.6
			COPY foo bar /
		`),
	)
	defer dir.Remove()

	options := newBuildOptions()
	options.context = dir.Path()

	err := runBuild(cli, options)
	assert.NilError(t, err)

	files, err := ioutil.ReadDir(dest.Path())
	assert.NilError(t, err)
	for _, fileInfo := range files {
		assert.Check(t, is.Equal(uint32(0), fileInfo.Sys().(*syscall.Stat_t).Uid))
		assert.Check(t, is.Equal(uint32(0), fileInfo.Sys().(*syscall.Stat_t).Gid))
	}
}
func TestRunBuildDockerfileFromStdinWithCompress(t *testing.T) {
	dest, err := ioutil.TempDir("", "test-build-compress-dest")
	assert.NilError(t, err)
	defer os.RemoveAll(dest)

	var dockerfileName string
	fakeImageBuild := func(_ context.Context, context io.Reader, options types.ImageBuildOptions) (types.ImageBuildResponse, error) {
		buffer := new(bytes.Buffer)
		tee := io.TeeReader(context, buffer)

		assert.Check(t, archive.Untar(tee, dest, nil))
		dockerfileName = options.Dockerfile

		header := buffer.Bytes()[:10]
		assert.Check(t, is.Equal(archive.Gzip, archive.DetectCompression(header)))

		body := new(bytes.Buffer)
		return types.ImageBuildResponse{Body: ioutil.NopCloser(body)}, nil
	}

	cli := test.NewFakeCli(&fakeClient{imageBuildFunc: fakeImageBuild})
	dockerfile := bytes.NewBufferString(`
		FROM alpine:3.6
		COPY foo /
	`)
	cli.SetIn(command.NewInStream(ioutil.NopCloser(dockerfile)))

	dir, err := ioutil.TempDir("", "test-build-compress")
	assert.NilError(t, err)
	defer os.RemoveAll(dir)

	ioutil.WriteFile(filepath.Join(dir, "foo"), []byte("some content"), 0644)

	options := newBuildOptions()
	options.compress = true
	options.dockerfileName = "-"
	options.context = dir

	err = runBuild(cli, options)
	assert.NilError(t, err)

	files, err := ioutil.ReadDir(dest)
	assert.NilError(t, err)
	actual := []string{}
	for _, fileInfo := range files {
		actual = append(actual, fileInfo.Name())
	}
	sort.Strings(actual)
	assert.Check(t, is.DeepEqual([]string{dockerfileName, ".dockerignore", "foo"}, actual))
}

func TestRunBuildDockerfileOutsideContext(t *testing.T) {
	dir := fs.NewDir(t, t.Name(),
		fs.WithFile("data", "data file"),
	)
	defer dir.Remove()

	// Dockerfile outside of build-context
	df := fs.NewFile(t, t.Name(),
		fs.WithContent(`
FROM FOOBAR
COPY data /data
		`),
	)
	defer df.Remove()

	dest, err := ioutil.TempDir("", t.Name())
	assert.NilError(t, err)
	defer os.RemoveAll(dest)

	var dockerfileName string
	fakeImageBuild := func(_ context.Context, context io.Reader, options types.ImageBuildOptions) (types.ImageBuildResponse, error) {
		buffer := new(bytes.Buffer)
		tee := io.TeeReader(context, buffer)

		assert.Check(t, archive.Untar(tee, dest, nil))
		dockerfileName = options.Dockerfile

		body := new(bytes.Buffer)
		return types.ImageBuildResponse{Body: ioutil.NopCloser(body)}, nil
	}

	cli := test.NewFakeCli(&fakeClient{imageBuildFunc: fakeImageBuild})

	options := newBuildOptions()
	options.context = dir.Path()
	options.dockerfileName = df.Path()

	err = runBuild(cli, options)
	assert.NilError(t, err)

	files, err := ioutil.ReadDir(dest)
	assert.NilError(t, err)
	var actual []string
	for _, fileInfo := range files {
		actual = append(actual, fileInfo.Name())
	}
	sort.Strings(actual)
	assert.Check(t, is.DeepEqual([]string{dockerfileName, ".dockerignore", "data"}, actual))
}

// TestRunBuildFromLocalGitHubDirNonExistingRepo tests that build contexts
// starting with `github.com/` are special-cased, and the build command attempts
// to clone the remote repo.
func TestRunBuildFromGitHubSpecialCase(t *testing.T) {
	cmd := NewBuildCommand(test.NewFakeCli(nil))
	cmd.SetArgs([]string{"github.com/docker/no-such-repository"})
	cmd.SetOutput(ioutil.Discard)
	err := cmd.Execute()
	assert.ErrorContains(t, err, "unable to prepare context: unable to 'git clone'")
}

// TestRunBuildFromLocalGitHubDirNonExistingRepo tests that a local directory
// starting with `github.com` takes precedence over the `github.com` special
// case.
func TestRunBuildFromLocalGitHubDir(t *testing.T) {
	tmpDir, err := ioutil.TempDir("", "docker-build-from-local-dir-")
	assert.NilError(t, err)
	defer os.RemoveAll(tmpDir)

	buildDir := filepath.Join(tmpDir, "github.com", "docker", "no-such-repository")
	err = os.MkdirAll(buildDir, 0777)
	assert.NilError(t, err)
	err = ioutil.WriteFile(filepath.Join(buildDir, "Dockerfile"), []byte("FROM busybox\n"), 0644)
	assert.NilError(t, err)

	client := test.NewFakeCli(&fakeClient{})
	cmd := NewBuildCommand(client)
	cmd.SetArgs([]string{buildDir})
	cmd.SetOutput(ioutil.Discard)
	err = cmd.Execute()
	assert.NilError(t, err)
}
