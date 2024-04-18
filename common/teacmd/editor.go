package teacmd

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rotisserie/eris"
	"golang.org/x/mod/modfile"

	"pkg.world.dev/world-cli/common/globalconfig"
	"pkg.world.dev/world-cli/common/logger"
)

const (
	TargetEditorDir = ".editor"

	releaseURL       = "https://api.github.com/repos/Argus-Labs/cardinal-editor/releases"
	latestReleaseURL = releaseURL + "/latest"

	httpTimeout = 2 * time.Second

	cardinalProjectIDPlaceholder = "__CARDINAL_PROJECT_ID__"

	cardinalGomodPath = "cardinal/go.mod"

	cardinalPkgPath = "pkg.world.dev/world-engine/cardinal"

	versionMapURL = "https://raw.githubusercontent.com/Argus-Labs/cardinal-editor/main/version_map.json"
)

var (
	// Cardinal : Cardinal Editor version map
	// This is the default value for fallback if cannot get version from repository
	defaultCardinalVersionMap = map[string]string{
		"v1.2.2-beta": "v0.1.0",
		"v1.2.3-beta": "v0.1.0",
		"v1.2.4-beta": "v0.3.1",
		"v1.2.5-beta": "v0.3.1",
	}
)

type Asset struct {
	BrowserDownloadURL string `json:"browser_download_url"`
}

type Release struct {
	Name   string  `json:"name"`
	Assets []Asset `json:"assets"`
}

func SetupCardinalEditor() error {
	// Get the version map
	cardinalVersionMap, err := getVersionMap(versionMapURL)
	if err != nil {
		logger.Warn("Failed to get version map, using default version map")
		cardinalVersionMap = defaultCardinalVersionMap
	}

	// Check version
	cardinalVersion, err := getModuleVersion(cardinalGomodPath, cardinalPkgPath)
	if err != nil {
		return eris.Wrap(err, "failed to get cardinal version")
	}

	downloadURL := latestReleaseURL
	downloadVersion, versionIsExist := cardinalVersionMap[cardinalVersion]
	if versionIsExist {
		downloadURL = fmt.Sprintf("%s/tags/%s", releaseURL, downloadVersion)
	}

	// Check if the Cardinal Editor directory exists
	if _, err := os.Stat(TargetEditorDir); !os.IsNotExist(err) {
		// Check the version of cardinal editor is appropriate
		if fileExists(filepath.Join(TargetEditorDir, downloadVersion)) {
			// do nothing if the version is already downloaded
			return nil
		}

		// Remove the existing Cardinal Editor directory
		os.RemoveAll(TargetEditorDir)
	}

	configDir, err := globalconfig.GetConfigDir()
	if err != nil {
		return err
	}

	editorDir, downloadVersion, err := downloadReleaseIfNotCached(downloadURL, configDir)
	if err != nil {
		return err
	}

	// rename version tag dir to .editor
	err = copyDir(editorDir, TargetEditorDir)
	if err != nil {
		return err
	}

	// rename project id
	// "ce" prefix is added because guids can start with numbers, which is not allowed in js
	projectID := "ce" + strippedGUID()
	err = replaceProjectIDs(TargetEditorDir, projectID)
	if err != nil {
		return err
	}

	// this file is used to check if the version is already downloaded
	err = addFileVersion(filepath.Join(TargetEditorDir, downloadVersion))
	if err != nil {
		return err
	}

	return nil
}

// downloadReleaseIfNotCached : downloads the latest release of the Cardinal Editor if it is not already cached.
// returns editor directory path, version and error
func downloadReleaseIfNotCached(downloadURL, configDir string) (string, string, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, downloadURL, nil)
	if err != nil {
		return "", "", err
	}

	client := &http.Client{
		Timeout: httpTimeout,
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	var release Release
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}
	if err = json.Unmarshal(bodyBytes, &release); err != nil {
		return "", "", err
	}

	editorDir := filepath.Join(configDir, "editor")

	targetDir := filepath.Join(editorDir, release.Name)
	if _, err = os.Stat(targetDir); os.IsNotExist(err) {
		return targetDir, release.Name, downloadAndUnzip(release.Assets[0].BrowserDownloadURL, targetDir)
	}

	return targetDir, release.Name, nil
}

func downloadAndUnzip(url string, targetDir string) error {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	client := &http.Client{
		Timeout: httpTimeout + 10*time.Second,
	}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New(url)
	}
	defer resp.Body.Close()

	tmpZipFileName := "tmp.zip"
	file, err := os.Create(tmpZipFileName)
	if err != nil {
		return err
	}
	defer file.Close()

	_, err = io.Copy(file, resp.Body)
	if err != nil {
		return err
	}

	if err = unzipFile(tmpZipFileName, targetDir); err != nil {
		return err
	}

	return os.Remove(tmpZipFileName)
}

func unzipFile(filename string, targetDir string) error {
	reader, err := zip.OpenReader(filename)
	if err != nil {
		return err
	}
	defer reader.Close()

	// save original folder name
	var originalDir string
	for i, file := range reader.File {
		if i == 0 {
			originalDir = file.Name
		}

		src, err := file.Open()
		if err != nil {
			return err
		}
		defer src.Close()

		filePath, err := sanitizeExtractPath(filepath.Dir(targetDir), file.Name)
		if err != nil {
			return err
		}
		if file.FileInfo().IsDir() {
			err = os.MkdirAll(filePath, 0755)
			if err != nil {
				return err
			}
			continue
		}

		dst, err := os.Create(filePath)
		if err != nil {
			return err
		}
		defer dst.Close()

		_, err = io.Copy(dst, src) //nolint:gosec // zip file is from us
		if err != nil {
			return err
		}
	}

	if err = os.Rename(filepath.Join(filepath.Dir(targetDir), originalDir), targetDir); err != nil {
		return err
	}

	return nil
}

func sanitizeExtractPath(dst string, filePath string) (string, error) {
	dstPath := filepath.Join(dst, filePath)
	if strings.HasPrefix(dstPath, filepath.Clean(dst)) {
		return dstPath, nil
	}
	return "", fmt.Errorf("%s: illegal file path", filePath)
}

func copyDir(src string, dst string) error {
	srcDir, err := os.ReadDir(src)
	if err != nil {
		return errors.New(src)
	}

	if err := os.MkdirAll(dst, 0755); err != nil {
		return err
	}

	for _, entry := range srcDir {
		srcPath := filepath.Join(src, entry.Name())
		destPath := filepath.Join(dst, entry.Name())

		if entry.IsDir() {
			// Recursively copy dirs
			if err := copyDir(srcPath, destPath); err != nil {
				return err
			}
		} else {
			if err := copyFile(srcPath, destPath); err != nil {
				return err
			}
		}
	}

	return nil
}

func copyFile(src, dest string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	destFile, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer destFile.Close()

	_, err = io.Copy(destFile, srcFile)
	if err != nil {
		return err
	}

	return nil
}

func replaceProjectIDs(editorDir string, newID string) error {
	assetsDir := filepath.Join(editorDir, "assets")
	files, err := os.ReadDir(assetsDir)
	if err != nil {
		return err
	}

	for _, file := range files {
		if !file.IsDir() && strings.HasSuffix(file.Name(), ".js") {
			content, err := os.ReadFile(filepath.Join(assetsDir, file.Name()))
			if err != nil {
				return err
			}

			newContent := strings.ReplaceAll(string(content), cardinalProjectIDPlaceholder, newID)

			err = os.WriteFile(filepath.Join(assetsDir, file.Name()), []byte(newContent), 0600)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func strippedGUID() string {
	u := uuid.New()
	return strings.ReplaceAll(u.String(), "-", "")
}

// addFileVersion add file with name version of cardinal editor
func addFileVersion(version string) error {
	// Create the file
	file, err := os.Create(version)
	if err != nil {
		return err
	}
	defer file.Close()

	return nil
}

func getModuleVersion(gomodPath, modulePath string) (string, error) {
	// Read the go.mod file
	data, err := os.ReadFile(gomodPath)
	if err != nil {
		return "", err
	}

	// Parse the go.mod file
	modFile, err := modfile.Parse(gomodPath, data, nil)
	if err != nil {
		return "", err
	}

	// Iterate through the require statements to find the desired module
	for _, require := range modFile.Require {
		if require.Mod.Path == modulePath {
			return require.Mod.Version, nil
		}
	}

	// Return an error if the module is not found
	return "", fmt.Errorf("module %s not found", modulePath)
}

// fileExists checks if a file exists and is not a directory before we
// try using it to prevent further errors.
func fileExists(filename string) bool {
	info, err := os.Stat(filename)
	if os.IsNotExist(err) {
		return false
	}
	return !info.IsDir()
}

// getVersionMap fetches the JSON data from a URL and unmarshals it into a map[string]string.
func getVersionMap(url string) (map[string]string, error) {
	// Make an HTTP GET request
	client := &http.Client{
		Timeout: httpTimeout,
	}

	// Create a new request using http
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	// Send the request via a client
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Check for HTTP error
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP error: %d - %s", resp.StatusCode, resp.Status)
	}

	// Read the response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// Unmarshal the JSON data into a map
	var result map[string]string
	err = json.Unmarshal(body, &result)
	if err != nil {
		return nil, err
	}

	return result, nil
}
