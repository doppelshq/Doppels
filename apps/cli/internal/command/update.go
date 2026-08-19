package command

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"doppels.so/cli/internal/version"
)

const githubReleasesURL = "https://api.github.com/repos/doppelshq/Doppels/releases?per_page=1"

type githubRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

func (app *App) runUpdate(arguments []string) int {
	flags := app.flagSet("update")
	jsonOutput := flags.Bool("json", false, "write a machine-readable response")
	if err := flags.Parse(arguments); err != nil {
		return ExitContract
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(app.Stderr, "update accepts no arguments")
		return ExitContract
	}

	executable, err := app.executable()
	if err != nil {
		fmt.Fprintf(app.Stderr, "resolve executable path: %v\n", err)
		return ExitOperational
	}

	if isManagedByBrew(executable) {
		if *jsonOutput {
			app.writeJSON(map[string]any{"status": "brew-managed", "hint": "brew upgrade doppelshq/tap/doppels"})
		} else {
			style := newTermStyle(app.Stdout)
			fmt.Fprintln(app.Stdout)
			fmt.Fprintf(app.Stdout, "  %s  installed via Homebrew\n", style.field("Manager"))
			fmt.Fprintf(app.Stdout, "  %s\n", style.dim("Run: brew upgrade doppelshq/tap/doppels"))
			fmt.Fprintln(app.Stdout)
		}
		return ExitSuccess
	}

	style := newTermStyle(app.Stdout)
	if !*jsonOutput {
		fmt.Fprintln(app.Stdout)
		fmt.Fprintf(app.Stdout, "  %s  checking latest release…\n", style.field("Update"))
	}

	release, err := fetchLatestRelease(app.HTTPClient)
	if err != nil {
		fmt.Fprintf(app.Stderr, "fetch release info: %v\n", err)
		return ExitOperational
	}

	latest := strings.TrimPrefix(release.TagName, "v")
	current := version.Version

	if current != "dev" && current == latest {
		if *jsonOutput {
			app.writeJSON(map[string]any{"status": "up-to-date", "version": current})
		} else {
			fmt.Fprintf(app.Stdout, "  %s  already on %s\n", style.field("Version"), style.value(current))
			fmt.Fprintln(app.Stdout)
		}
		return ExitSuccess
	}

	assetName := fmt.Sprintf("doppels_%s_%s_%s.tar.gz", latest, runtime.GOOS, runtime.GOARCH)
	downloadURL := ""
	for _, asset := range release.Assets {
		if asset.Name == assetName {
			downloadURL = asset.BrowserDownloadURL
			break
		}
	}
	if downloadURL == "" {
		fmt.Fprintf(app.Stderr, "no release asset found for %s/%s (looked for %s)\n", runtime.GOOS, runtime.GOARCH, assetName)
		return ExitOperational
	}

	if !*jsonOutput {
		fmt.Fprintf(app.Stdout, "  %s  %s → %s\n", style.field("Release"), style.dim(current), style.value(latest))
		fmt.Fprintf(app.Stdout, "  %s  downloading…\n", style.field("Asset"))
	}

	newBinary, err := downloadBinary(app.HTTPClient, downloadURL)
	if err != nil {
		fmt.Fprintf(app.Stderr, "download binary: %v\n", err)
		return ExitOperational
	}
	defer os.Remove(newBinary)

	if err := replaceExecutable(executable, newBinary); err != nil {
		fmt.Fprintf(app.Stderr, "replace executable: %v\n", err)
		return ExitOperational
	}

	if *jsonOutput {
		app.writeJSON(map[string]any{"status": "updated", "from": current, "to": latest})
	} else {
		fmt.Fprintf(app.Stdout, "  %s  %s\n", style.field("Updated"), style.value("doppels "+latest))
		fmt.Fprintln(app.Stdout)
	}
	return ExitSuccess
}

func fetchLatestRelease(client *http.Client) (*githubRelease, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequest(http.MethodGet, githubReleasesURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}
	var releases []githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return nil, err
	}
	if len(releases) == 0 {
		return nil, fmt.Errorf("no releases found")
	}
	return &releases[0], nil
}

func downloadBinary(client *http.Client, url string) (string, error) {
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download returned %d", resp.StatusCode)
	}

	gr, err := gzip.NewReader(resp.Body)
	if err != nil {
		return "", fmt.Errorf("gzip: %w", err)
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("tar: %w", err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		base := filepath.Base(header.Name)
		if base != "doppels" && base != "doppels.exe" {
			continue
		}
		tmp, err := os.CreateTemp("", "doppels-update-*")
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(tmp, tr); err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return "", err
		}
		tmp.Close()
		if err := os.Chmod(tmp.Name(), 0o755); err != nil {
			os.Remove(tmp.Name())
			return "", err
		}
		return tmp.Name(), nil
	}
	return "", fmt.Errorf("binary not found inside archive")
}

func replaceExecutable(target, replacement string) error {
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".doppels-update-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpName)

	src, err := os.Open(replacement)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(tmpName, os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	dst.Close()

	return os.Rename(tmpName, target)
}

func isManagedByBrew(path string) bool {
	return strings.Contains(path, "/Cellar/") || strings.Contains(path, "/homebrew/")
}

func (app *App) executable() (string, error) {
	if app.Executable != nil {
		return app.Executable()
	}
	return os.Executable()
}
