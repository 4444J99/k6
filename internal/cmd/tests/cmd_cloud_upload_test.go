package tests

import (
	"os"
	"path/filepath"
	"testing"

	"go.k6.io/k6/internal/cmd"
	"go.k6.io/k6/lib/fsext"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestK6CloudUpload(t *testing.T) {
	t.Parallel()

	t.Run("TestCloudUploadUserNotAuthenticated", func(t *testing.T) {
		t.Parallel()

		opts := defaultV6MockOpts()
		ts := getSimpleCloudTestStateV6(t, nil, setupK6CloudUploadCmd, nil, opts)
		delete(ts.Env, "K6_CLOUD_TOKEN")
		ts.ExpectedExitCode = -1
		cmd.ExecuteWithGlobalState(ts.GlobalState)

		stdout := ts.Stdout.String()
		t.Log(stdout)
		assert.Contains(t, stdout, `must first authenticate`)
	})

	t.Run("TestCloudUploadWithScript", func(t *testing.T) {
		t.Parallel()

		opts := defaultV6MockOpts()
		ts := getSimpleCloudTestStateV6(t, nil, setupK6CloudUploadCmd, nil, opts)
		cmd.ExecuteWithGlobalState(ts.GlobalState)

		stdout := ts.Stdout.String()
		t.Log(stdout)
		assert.Contains(t, stdout, `test status: Uploaded`)
	})

	t.Run("TestCloudUploadWithArchive", func(t *testing.T) {
		t.Parallel()

		opts := defaultV6MockOpts()
		opts.projectID = 124 // matches the archive's embedded projectID
		srv := getMockCloudV6(t, opts)

		ts := NewGlobalTestState(t)

		data, err := os.ReadFile(filepath.Join("testdata/archives", "archive_v0.46.0_with_loadimpact_option.tar")) //nolint:forbidigo // it's a test
		require.NoError(t, err)
		require.NoError(t, fsext.WriteFile(ts.FS, filepath.Join(ts.Cwd, "archive.tar"), data, 0o644))

		ts.CmdArgs = []string{"k6", "cloud", "upload", "archive.tar"}
		ts.Env["K6_SHOW_CLOUD_LOGS"] = "false"
		ts.Env["K6_CLOUD_HOST_V6"] = srv.URL
		ts.Env["K6_CLOUD_HOST"] = srv.URL
		ts.Env["K6_CLOUD_TOKEN"] = "foo"
		ts.Env["K6_CLOUD_STACK_ID"] = "123"

		cmd.ExecuteWithGlobalState(ts.GlobalState)

		stdout := ts.Stdout.String()
		t.Log(stdout)
		assert.NotContains(t, stdout, `not logged in`)
		assert.Contains(t, stdout, `test status: Uploaded`)
	})
}

func setupK6CloudUploadCmd(cliFlags []string) []string {
	return append([]string{"k6", "cloud", "upload"}, append(cliFlags, "test.js")...)
}
