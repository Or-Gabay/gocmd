package cmd

import (
	"errors"
	"fmt"
	"github.com/jfrog/gocmd/executers/utils"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	gofrogcmd "github.com/jfrog/gofrog/io"
	"github.com/jfrog/jfrog-client-go/auth"
	"github.com/jfrog/jfrog-client-go/utils/errorutils"
	"github.com/jfrog/jfrog-client-go/utils/io/fileutils"
	"github.com/jfrog/jfrog-client-go/utils/log"
)

// Used for masking basic auth credentials as part of a URL.
var protocolRegExp *gofrogcmd.CmdOutputPattern

func NewCmd() (*Cmd, error) {
	execPath, err := exec.LookPath("go")
	if err != nil {
		return nil, errorutils.CheckError(err)
	}
	return &Cmd{Go: execPath}, nil
}

func (config *Cmd) GetCmd() (cmd *exec.Cmd) {
	var cmdStr []string
	cmdStr = append(cmdStr, config.Go)
	cmdStr = append(cmdStr, config.Command...)
	cmdStr = append(cmdStr, config.CommandFlags...)
	cmd = exec.Command(cmdStr[0], cmdStr[1:]...)
	cmd.Dir = config.Dir
	return
}

func (config *Cmd) GetEnv() map[string]string {
	return map[string]string{}
}

func (config *Cmd) GetStdWriter() io.WriteCloser {
	return config.StrWriter
}

func (config *Cmd) GetErrWriter() io.WriteCloser {
	return config.ErrWriter
}

type Cmd struct {
	Go           string
	Command      []string
	CommandFlags []string
	Dir          string
	StrWriter    io.WriteCloser
	ErrWriter    io.WriteCloser
}

func GetGoVersion() (string, error) {
	goCmd, err := NewCmd()
	if err != nil {
		return "", err
	}
	goCmd.Command = []string{"version"}
	output, err := gofrogcmd.RunCmdOutput(goCmd)
	return output, errorutils.CheckError(err)
}

func RunGo(goArg []string, server auth.ServiceDetails, repo string) error {
	utils.SetGoProxyWithApi(repo, server)

	goCmd, err := NewCmd()
	if err != nil {
		return err
	}
	goCmd.Command = goArg
	err = prepareRegExp()
	if err != nil {
		return err
	}

	performPasswordMask, err := shouldMaskPassword()
	if err != nil {
		return err
	}
	if performPasswordMask {
		_, _, _, err = gofrogcmd.RunCmdWithOutputParser(goCmd, true, protocolRegExp)
	} else {
		_, _, _, err = gofrogcmd.RunCmdWithOutputParser(goCmd, true)
	}
	return errorutils.CheckError(err)
}

// GetGOPATH returns the location of the GOPATH
func getGOPATH() (string, error) {
	goCmd, err := NewCmd()
	if err != nil {
		return "", errorutils.CheckError(err)
	}
	goCmd.Command = []string{"env", "GOPATH"}
	output, err := gofrogcmd.RunCmdOutput(goCmd)
	if errorutils.CheckError(err) != nil {
		return "", fmt.Errorf("Could not find GOPATH env: %s", err.Error())
	}
	return strings.TrimSpace(parseGoPath(string(output))), nil
}

// Using go mod download {dependency} command to download the dependency
func DownloadDependency(dependencyName string) error {
	goCmd, err := NewCmd()
	if err != nil {
		return err
	}
	log.Debug("Running go mod download -json", dependencyName)
	goCmd.Command = []string{"mod", "download", "-json", dependencyName}
	return errorutils.CheckError(gofrogcmd.RunCmd(goCmd))
}

// Runs 'go list -m all' command and returns map of the dependencies
func GetDependenciesList(projectDir string) (map[string]bool, error) {
	var err error
	if projectDir == "" {
		projectDir, err = GetProjectRoot()
		if err != nil {
			return nil, err
		}
	}

	// Read and store the details of the go.mod and go.sum files,
	// because they may change by the "go list" command.
	modFileContent, modFileStat, err := GetFileDetails(filepath.Join(projectDir, "go.mod"))
	if err != nil {
		log.Info("Dependencies were not collected for this build, since go.mod could not be found in", projectDir)
		return nil, nil
	}
	sumFileContent, sumFileStat, err := GetGoSum(projectDir)
	if len(sumFileContent) > 0 && sumFileStat != nil {
		defer RestoreSumFile(projectDir, sumFileContent, sumFileStat)
	}

	log.Info("Running 'go list -m all' in", projectDir)
	goCmd, err := NewCmd()
	if err != nil {
		return nil, err
	}
	isAutoModify, err := automaticallyModifyMod()
	if err != nil {
		return nil, err
	}
	// Since version go1.16 build commands (like go build and go list) no longer modify go.mod and go.sum by default.
	if isAutoModify {
		goCmd.Command = []string{"list", "-m", "all"}
	} else {
		goCmd.Command = []string{"list", "-m", "-mod=mod", "all"}
	}
	goCmd.Dir = projectDir

	err = prepareGlobalRegExp()
	if err != nil {
		return nil, err
	}

	performPasswordMask, err := shouldMaskPassword()
	if err != nil {
		return nil, err
	}
	var output string
	var executionError error
	if performPasswordMask {
		output, _, _, executionError = gofrogcmd.RunCmdWithOutputParser(goCmd, false, protocolRegExp)
	} else {
		output, _, _, executionError = gofrogcmd.RunCmdWithOutputParser(goCmd, false)
	}

	if len(output) != 0 {
		log.Debug(output)
	}

	if executionError != nil {
		// If the command fails, the mod stays the same, therefore, don't need to be restored.
		return nil, errorutils.CheckError(executionError)
	}

	// Restore the the go.mod and go.sum files, to make sure they stay the same as before
	// running the "go list" command.
	err = ioutil.WriteFile(filepath.Join(projectDir, "go.mod"), modFileContent, modFileStat.Mode())
	if err != nil {
		return nil, err
	}

	return outputToMap(output), errorutils.CheckError(err)
}

// Returns the root dir where the go.mod located.
func GetProjectRoot() (string, error) {
	// Create a map to store all paths visited, to avoid running in circles.
	visitedPaths := make(map[string]bool)
	// Get the current directory.
	wd, err := os.Getwd()
	if err != nil {
		return wd, errorutils.CheckError(err)
	}
	defer os.Chdir(wd)

	// Get the OS root.
	osRoot := os.Getenv("SYSTEMDRIVE")
	if osRoot != "" {
		// If this is a Windows machine:
		osRoot += "\\"
	} else {
		// Unix:
		osRoot = "/"
	}

	// Check if the current directory includes the go.mod file. If not, check the parent directpry
	// and so on.
	for {
		// If the go.mod is found the current directory, return the path.
		exists, err := fileutils.IsFileExists(filepath.Join(wd, "go.mod"), false)
		if err != nil || exists {
			return wd, err
		}

		// If this the OS root, we can stop.
		if wd == osRoot {
			break
		}

		// Save this path.
		visitedPaths[wd] = true
		// CD to the parent directory.
		wd = filepath.Dir(wd)
		os.Chdir(wd)

		// If we already visited this directory, it means that there's a loop and we can stop.
		if visitedPaths[wd] {
			return "", errorutils.CheckError(errors.New("Could not find go.mod for project."))
		}
	}

	return "", errorutils.CheckError(errors.New("Could not find go.mod for project."))
}

// GetGoModCachePath returns the location of the go module cache
func GetGoModCachePath() (string, error) {
	goPath, err := getGOPATH()
	if err != nil {
		return "", err
	}
	return filepath.Join(goPath, "pkg", "mod"), nil
}

// GetCachePath returns the location of downloads dir insied the GOMODCACHE
func GetCachePath() (string, error) {
	goModCachePath, err := GetGoModCachePath()
	if err != nil {
		return "", err
	}
	return filepath.Join(goModCachePath, "cache", "download"), nil
}

func parseGoPath(goPath string) string {
	if runtime.GOOS == "windows" {
		goPathSlice := strings.Split(goPath, ";")
		return goPathSlice[0]
	}
	goPathSlice := strings.Split(goPath, ":")
	return goPathSlice[0]
}
