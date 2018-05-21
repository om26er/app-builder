package download

import (
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/alecthomas/kingpin"
	"github.com/apex/log"
	"github.com/develar/app-builder/pkg/util"
	"github.com/develar/errors"
	"github.com/mitchellh/go-homedir"
)

func ConfigureArtifactCommand(app *kingpin.Application) {
	command := app.Command("download-artifact", "Download, unpack and cache artifact from GitHub.")
	name := command.Flag("name", "The artifact name.").Short('n').Required().String()
	url := command.Flag("url", "The artifact URL.").Short('u').Required().String()
	sha512 := command.Flag("sha512", "The expected sha512 of file.").String()

	command.Action(func(context *kingpin.ParseContext) error {
		dirPath, err := DownloadArtifact(*name, *url, *sha512)
		if err != nil {
			return errors.WithStack(err)
		}
		_, err = os.Stdout.Write([]byte(dirPath))
		return errors.WithStack(err)
	})
}

// we cache in the global location - in the home dir, not in the node_modules/.cache (https://www.npmjs.com/package/find-cache-dir) because
// * don't need to find node_modules
// * don't pollute user project dir (important in case of 1-package.json project structure)
// * simplify/speed-up tests (don't download fpm for each test project)
func DownloadArtifact(dirName string, url string, checksum string) (string, error) {
	if dirName == "fpm" {
		return DownloadFpm()
	} else if dirName == "zstd" {
		return DownloadZstd(runtime.GOOS)
	}

	isNodeJsArtifact := dirName == "node"
	if isNodeJsArtifact {
		versionAndArch := url
		version := versionAndArch[0:strings.Index(versionAndArch, "-")]
		url = "https://nodejs.org/dist/v" + version + "/node-v" + versionAndArch + ".tar.xz"
		dirName = dirName + "-" + versionAndArch
	}

	if len(dirName) == 0 {
		fileName := path.Base(url)
		dirName = strings.TrimSuffix(fileName, path.Ext(fileName))
	}

	cacheDir, err := GetCacheDirectory("electron-builder")
	if err != nil {
		return "", errors.WithStack(err)
	}

	hyphenIndex := strings.Index(dirName, "-")
	if hyphenIndex > 0 {
		cacheDir = filepath.Join(cacheDir, dirName[0:hyphenIndex])
	} else {
		cacheDir = filepath.Join(cacheDir, dirName)
	}
	filePath := filepath.Join(cacheDir, dirName)

	logFields := log.Fields{
		"path": filePath,
	}

	dirStat, err := os.Stat(filePath)
	if err == nil && (dirStat.IsDir() || strings.HasSuffix(filePath, ".tar")) {
		log.WithFields(logFields).Debug("found existing")
		return filePath, nil
	}

	if err != nil && !os.IsNotExist(err) {
		return "", errors.WithMessage(err, "error during cache check for path "+filePath)
	}

	err = os.MkdirAll(cacheDir, 0777)
	if err != nil {
		return "", errors.WithStack(err)
	}

	log.WithFields(logFields).WithField("url", url).Info("downloading")

	// 7z cannot be extracted from the input stream, temp file is required
	tempUnpackDir, err := util.TempDir(cacheDir, "")
	if err != nil {
		return "", errors.WithStack(err)
	}

	var archiveName string
	if isNodeJsArtifact {
		archiveName = tempUnpackDir + ".tar.xz"
	} else {
		archiveName = tempUnpackDir + ".7z"
	}

	err = NewDownloader().Download(url, archiveName, checksum)
	if err != nil {
		return "", errors.WithStack(err)
	}

	if isNodeJsArtifact {
		err = unpackTarXz(archiveName, tempUnpackDir)
		if err != nil {
			return "", errors.WithStack(err)
		}
	} else {
		command := exec.Command(util.GetEnvOrDefault("SZA_PATH", "7za"), "x", "-bd", archiveName, "-o"+tempUnpackDir)
		command.Dir = cacheDir
		output, err := command.CombinedOutput()
		if err != nil {
			return "", errors.WithStack(err)
		}

		log.Debug(string(output))
	}

	err = os.Remove(archiveName)
	if err != nil {
		return "", errors.WithStack(err)
	}

	if strings.HasSuffix(url, ".tar.7z") {
		err = os.Rename(filepath.Join(tempUnpackDir, filepath.Base(tempUnpackDir)), filePath)
		os.RemoveAll(tempUnpackDir)
	} else {
		err = os.Rename(tempUnpackDir, filePath)
	}

	if err != nil {
		log.WithFields(logFields).WithFields(log.Fields{
			"tempUnpackDir": tempUnpackDir,
			"error":         err,
		}).Warn("cannot move downloaded into final location (another process downloaded faster?)")
	}

	log.WithFields(logFields).Debug("downloaded")

	return filePath, nil
}

func unpackTarXz(archiveName string, unpackDir string) error {
	xzDecompressCommand := exec.Command(util.GetEnvOrDefault("SZA_PATH", "7za"), "e", "-bd", "-txz", archiveName, "-so")
	xzDecompressCommand.Stderr = os.Stderr

	xzStdout, err := xzDecompressCommand.StdoutPipe()
	if nil != err {
		return errors.WithStack(err)
	}

	err = xzDecompressCommand.Start()
	if err != nil {
		return errors.WithStack(err)
	}

	//noinspection SpellCheckingInspection
	tarDecompressCommand := exec.Command(util.GetEnvOrDefault("SZA_PATH", "7za"), "e", "-bd", "-ttar", "-o"+unpackDir, "*/bin/node", "-r", "-si")
	tarDecompressCommand.Stderr = os.Stderr

	tarStdin, err := tarDecompressCommand.StdinPipe()
	if nil != err {
		return errors.WithStack(err)
	}

	err = tarDecompressCommand.Start()
	if err != nil {
		return errors.WithStack(err)
	}

	go func() {
		defer tarStdin.Close()
		io.Copy(tarStdin, xzStdout)
	}()

	err = xzDecompressCommand.Wait()
	if err != nil {
		return errors.WithStack(err)
	}

	err = tarDecompressCommand.Wait()
	if err != nil {
		return errors.WithStack(err)
	}

	err = os.Chmod(filepath.Join(unpackDir, "node"), 0755)
	if err != nil {
		return errors.WithStack(err)
	}
	return nil
}

func DownloadCompressedArtifact(subDir string, url string, checksum string) (string, error) {
	cacheDir, err := GetCacheDirectory("electron-builder")
	if err != nil {
		return "", errors.WithStack(err)
	}

	if len(subDir) != 0 {
		cacheDir = filepath.Join(cacheDir, subDir)
	}

	filePath := filepath.Join(cacheDir, path.Base(url))
	logFields := log.Fields{
		"file": filePath,
	}

	_, err = os.Stat(filePath)
	if err == nil {
		log.WithFields(logFields).Debug("found existing")
		return filePath, nil
	}

	if err != nil && !os.IsNotExist(err) {
		return "", errors.WithMessage(err, "error during cache check for path "+filePath)
	}

	err = os.MkdirAll(cacheDir, 0777)
	if err != nil {
		return "", errors.WithStack(err)
	}

	tempFile, err := util.TempFile(cacheDir, ".snap")
	if err != nil {
		return "", errors.WithStack(err)
	}

	log.WithFields(logFields).WithField("url", url).Info("downloading")
	err = NewDownloader().Download(url, tempFile, checksum)
	if err != nil {
		return "", errors.WithStack(err)
	}
	log.WithFields(logFields).Debug("downloaded")

	err = os.Rename(tempFile, filePath)
	if err != nil {
		return "", errors.WithStack(err)
	}

	return filePath, nil
}

func GetCacheDirectory(dirName string) (string, error) {
	env := os.Getenv("ELECTRON_BUILDER_CACHE")
	if len(env) != 0 {
		return env, nil
	}

	if runtime.GOOS == "darwin" {
		userHomeDir, err := homedir.Dir()
		if err != nil {
			return "", errors.WithStack(err)
		}
		return filepath.Join(userHomeDir, "Library", "Caches", dirName), nil
	}

	localAppData := os.Getenv("LOCALAPPDATA")
	if runtime.GOOS == "windows" && len(localAppData) != 0 {
		// https://github.com/electron-userland/electron-builder/issues/1164
		if strings.Contains(strings.ToLower(localAppData), "\\windows\\system32\\") || strings.ToLower(os.Getenv("USERNAME")) == "system" {
			return filepath.Join(os.TempDir(), dirName+"-cache"), nil
		}
		return filepath.Join(localAppData, dirName, "cache"), nil
	}

	userHomeDir, err := homedir.Dir()
	if err != nil {
		return "", errors.WithStack(err)
	}
	return filepath.Join(userHomeDir, ".cache", "electron-builder"), nil
}

func DownloadFpm() (string, error) {
	if runtime.GOOS == "linux" {
		var checksum string
		var archSuffix string
		if runtime.GOARCH == "amd64" {
			checksum = "fcKdXPJSso3xFs5JyIJHG1TfHIRTGDP0xhSBGZl7pPZlz4/TJ4rD/q3wtO/uaBBYeX0qFFQAFjgu1uJ6HLHghA=="
			archSuffix = "-x86_64"
		} else {
			//noinspection SpellCheckingInspection
			checksum = "OnzvBdsHE5djcXcAT87rwbnZwS789ZAd2ehuIO42JWtBAHNzXKxV4o/24XFX5No4DJWGO2YSGQttW+zn7d/4rQ=="
			archSuffix = "-x86"
		}

		//noinspection SpellCheckingInspection
		name := "fpm-1.9.3-2.3.1-linux" + archSuffix
		return DownloadArtifact(
			name,
			"https://github.com/electron-userland/electron-builder-binaries/releases/download/" + name + "/" + name + ".7z",
			checksum,
		)
	} else {
		//noinspection SpellCheckingInspection
		return DownloadArtifact(
			"fpm-1.9.3-20150715-2.2.2-mac",
			"https://github.com/electron-userland/electron-builder-binaries/releases/download/fpm-1.9.3-20150715-2.2.2-mac/fpm-1.9.3-20150715-2.2.2-mac.7z",
			"oXfq+0H2SbdrbMik07mYloAZ8uHrmf6IJk+Q3P1kwywuZnKTXSaaeZUJNlWoVpRDWNu537YxxpBQWuTcF+6xfw==",
		)
	}

	return "", nil
}

func DownloadZstd(osName string) (string, error) {
	//noinspection SpellCheckingInspection
	return DownloadTool(ToolDescriptor{
		name: "zstd",
		version: "1.3.4",
		mac: "pLrLk2FAkop3C2drZ7+oxyGPQJjNMzUmVf0m3ZCc1a3WIEjYJNpq9UYvfBU/dl2CsRAchlKvoIOWRxRIdX0ugA==",
		linux: map[string]string{
			"x64": "C1TcuuN/0nNvHMwfkKmE8rgsDxkeSbGoV4DMSf4kIJIO4mNp+PUayYeBf4h3usScsWfvX70Jvg5v3yt1FySTDg==",
		},
		win: map[string]string{
			"ia32": "URJhIibWZUEy9USYlHBjc6bgEp7KP+hMJl/YWsssMTt6umxgk+niyc5meKs2XwOwBsvK6KsP+Qr/BawK7CdWVQ==",
			"x64": "S4RtWJwccUQfr/UQeZuWTJyJvU5uaYaP3rGT6e55epuAJx+fuljbJTBw+n8da0oRLIw0essEjGHkNafWgmKt1w==",
		},
	}, osName)
}

func DownloadTool(descriptor ToolDescriptor, osName string) (string, error) {
	arch := runtime.GOARCH
	if arch == "arm" {
		arch = "armv7"
	} else if arch == "arm64" {
		arch = "armv8"
	} else if arch == "amd64" {
		arch = "x64"
	}

	var checksum string
	var archQualifier string
	var osQualifier string
	if osName == "darwin" {
		checksum = descriptor.mac
		archQualifier = ""
		osQualifier = "mac"
	} else {
		archQualifier = "-" + arch
		if osName == "win32" {
			osQualifier = "win"
			checksum = descriptor.win[arch]
		} else {
			osQualifier = "linux"
			checksum = descriptor.linux[arch]
		}
	}

	if checksum == "" {
		return "", errors.Errorf("Checksum not specified for %s:%s", osName, arch)
	}

	repository := descriptor.repository
	if repository == "" {
		repository = "electron-userland/electron-builder-binaries"
	}

	var tagPrefix string
	if descriptor.repository == "" {
		tagPrefix = descriptor.name + "-"
	} else {
		tagPrefix = "v"
	}

	osAndArch := osQualifier + archQualifier
	return DownloadArtifact(
		descriptor.name+"-"+descriptor.version+"-"+osAndArch /* ability to use cache dir on any platform (e.g. keep cache under project) */,
		"https://github.com/"+repository+"/releases/download/"+tagPrefix+descriptor.version+"/"+descriptor.name+"-v"+descriptor.version+"-"+osAndArch+".7z",
		checksum,
	)
}

type ToolDescriptor struct {
	name string
	version string

	repository string

	mac string
	linux map[string]string
	win map[string]string
}