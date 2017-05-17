package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strings"

	"github.com/cyverse-de/messaging"
	"github.com/cyverse-de/model"
	"github.com/cyverse-de/road-runner/dcompose"
	"github.com/cyverse-de/road-runner/fs"
	"github.com/kr/pty"
	"github.com/pkg/errors"
	"github.com/spf13/viper"
)

// JobRunner provides the functionality needed to run jobs.
type JobRunner struct {
	client     JobUpdatePublisher
	exit       chan messaging.StatusCode
	job        *model.Job
	status     messaging.StatusCode
	cfg        *viper.Viper
	logsDir    string
	volumeDir  string
	workingDir string
}

// NewJobRunner creates a new JobRunner
func NewJobRunner(client JobUpdatePublisher, job *model.Job, cfg *viper.Viper, exit chan messaging.StatusCode) (*JobRunner, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	runner := &JobRunner{
		client:     client,
		exit:       exit,
		job:        job,
		cfg:        cfg,
		status:     messaging.Success,
		workingDir: cwd,
		volumeDir:  path.Join(cwd, dcompose.VOLUMEDIR),
		logsDir:    path.Join(cwd, dcompose.VOLUMEDIR, "logs"),
	}
	return runner, nil
}

// Init will initialize the state for a JobRunner. The volumeDir and logsDir
// will get created.
func (r *JobRunner) Init() error {
	err := os.MkdirAll(r.logsDir, 0755)
	if err != nil {
		return err
	}

	err = fs.CopyFile(fs.FS, "docker-compose.yml", path.Join(r.logsDir, "docker-compose.yml"))
	if err != nil {
		return err
	}

	transferTrigger, err := os.Create(path.Join(r.logsDir, "de-transfer-trigger.log"))
	if err != nil {
		return err
	}
	defer transferTrigger.Close()
	_, err = transferTrigger.WriteString("This is only used to force HTCondor to transfer files.")
	if err != nil {
		return err
	}

	if _, err = os.Stat("iplant.cmd"); err != nil {
		if err = os.Rename("iplant.cmd", path.Join(r.logsDir, "iplant.cmd")); err != nil {
			return err
		}
	}

	return nil
}

// DockerLogin will run "docker login" with credentials sent with the job.
func (r *JobRunner) DockerLogin() error {
	var err error
	dockerBin := r.cfg.GetString("docker.path")
	// Login so that images can be pulled.
	var authinfo *authInfo
	for _, img := range r.job.ContainerImages() {
		if img.Auth != "" {
			authinfo, err = parse(img.Auth)
			if err != nil {
				return err
			}
			//authCommand := exec.Command(dockerBin, "run", "--rm", "-it", "-v", "/var/run/docker.sock:/var/run/docker.sock", "-v", fmt.Sprintf("%s:%s", dockerCfg, "/root/.docker"), dockerimage, "login", "--username", authinfo.Username, "--password", authinfo.Password, parseRepo(img.Name))
			authCommand := exec.Command(dockerBin, "login", "--username", authinfo.Username, "--password", authinfo.Password, parseRepo(img.Name))
			f, err := pty.Start(authCommand)
			if err != nil {
				return err
			}
			go func() {
				io.Copy(log.Writer(), f)
			}()
			err = authCommand.Wait()
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// JobUpdatePublisher is the interface for types that need to publish a job
// update.
type JobUpdatePublisher interface {
	PublishJobUpdate(m *messaging.UpdateMessage) error
}

func (r *JobRunner) createDataContainers() (messaging.StatusCode, error) {
	var (
		err error
	)
	composePath := r.cfg.GetString("docker-compose.path")
	for index := range r.job.DataContainers() {
		running(r.client, r.job, fmt.Sprintf("creating data container data_%d", index))
		dataCommand := exec.Command(composePath, "-f", "docker-compose.yml", "up", fmt.Sprintf("data_%d", index))
		dataCommand.Env = os.Environ()
		dataCommand.Stderr = log.Writer()
		dataCommand.Stdout = log.Writer()
		if err = dataCommand.Run(); err != nil {
			running(r.client, r.job, fmt.Sprintf("error creating data container data_%d: %s", index, err.Error()))
			return messaging.StatusDockerCreateFailed, errors.Wrapf(err, "failed to create data container data_%d", index)
		}
		running(r.client, r.job, fmt.Sprintf("finished creating data container data_%d", index))
	}
	return messaging.Success, nil
}

func (r *JobRunner) downloadInputs() (messaging.StatusCode, error) {
	var (
		err      error
		exitCode int64
	)
	env := os.Environ()
	env = append(env, fmt.Sprintf("VAULT_ADDR=%s", r.cfg.GetString("vault.url")))
	env = append(env, fmt.Sprintf("VAULT_TOKEN=%s", r.cfg.GetString("vault.token")))
	composePath := r.cfg.GetString("docker-compose.path")
	for index, input := range r.job.Inputs() {
		running(r.client, r.job, fmt.Sprintf("Downloading %s", input.IRODSPath()))
		downloadCommand := exec.Command(composePath, "-f", "docker-compose.yml", "up", fmt.Sprintf("input_%d", index))
		downloadCommand.Env = env
		downloadCommand.Stderr = log.Writer()
		downloadCommand.Stdout = log.Writer()
		if err = downloadCommand.Run(); err != nil {
			running(r.client, r.job, fmt.Sprintf("error downloading %s: %s", input.IRODSPath(), err.Error()))
			return messaging.StatusInputFailed, errors.Wrapf(err, "failed to download %s with an exit code of %d", input.IRODSPath(), exitCode)
		}
		running(r.client, r.job, fmt.Sprintf("finished downloading %s", input.IRODSPath()))
	}
	return messaging.Success, nil
}

type authInfo struct {
	Username string
	Password string
}

func parse(b64 string) (*authInfo, error) {
	jsonstring, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, err
	}
	a := &authInfo{}
	err = json.Unmarshal(jsonstring, a)
	return a, err
}

func (r *JobRunner) runAllSteps() (messaging.StatusCode, error) {
	var err error

	for idx, step := range r.job.Steps {
		running(r.client, r.job,
			fmt.Sprintf(
				"Running tool container %s:%s with arguments: %s",
				step.Component.Container.Image.Name,
				step.Component.Container.Image.Tag,
				strings.Join(step.Arguments(), " "),
			),
		)
		composePath := r.cfg.GetString("docker-compose.path")
		runCommand := exec.Command(composePath, "-f", "docker-compose.yml", "up", fmt.Sprintf("step_%d", idx))
		runCommand.Env = os.Environ()
		runCommand.Stdout = log.Writer()
		runCommand.Stderr = log.Writer()
		err = runCommand.Run()

		if err != nil {
			running(r.client, r.job,
				fmt.Sprintf(
					"Error running tool container %s:%s with arguments '%s': %s",
					step.Component.Container.Image.Name,
					step.Component.Container.Image.Tag,
					strings.Join(step.Arguments(), " "),
					err.Error(),
				),
			)
			return messaging.StatusStepFailed, err
		}

		running(r.client, r.job,
			fmt.Sprintf("Tool container %s:%s with arguments '%s' finished successfully",
				step.Component.Container.Image.Name,
				step.Component.Container.Image.Tag,
				strings.Join(step.Arguments(), " "),
			),
		)
	}
	return messaging.Success, err
}

func (r *JobRunner) uploadOutputs() (messaging.StatusCode, error) {
	var err error
	composePath := r.cfg.GetString("docker-compose.path")
	outputCommand := exec.Command(composePath, "-f", "docker-compose.yml", "up", "upload_outputs")
	outputCommand.Env = []string{
		fmt.Sprintf("VAULT_ADDR=%s", r.cfg.GetString("vault.url")),
		fmt.Sprintf("VAULT_TOKEN=%s", r.cfg.GetString("vault.token")),
	}
	outputCommand.Stdout = log.Writer()
	outputCommand.Stderr = log.Writer()
	err = outputCommand.Run()

	if err != nil {
		running(r.client, r.job, fmt.Sprintf("Error uploading outputs to %s: %s", r.job.OutputDirectory(), err.Error()))
		return messaging.StatusOutputFailed, errors.Wrapf(err, "failed to upload outputs to %s", r.job.OutputDirectory())
	}

	running(r.client, r.job, fmt.Sprintf("Done uploading outputs to %s", r.job.OutputDirectory()))
	return messaging.Success, nil
}

func parseRepo(imagename string) string {
	if strings.Contains(imagename, "/") {
		parts := strings.Split(imagename, "/")
		return parts[0]
	}
	return ""
}

// Run executes the job, and returns the exit code on the exit channel.
func Run(client JobUpdatePublisher, job *model.Job, cfg *viper.Viper, exit chan messaging.StatusCode) {
	host, err := os.Hostname()
	if err != nil {
		log.Error(err)
		host = "UNKNOWN"
	}

	runner, err := NewJobRunner(client, job, cfg, exit)
	if err != nil {
		log.Error(err)
	}

	err = runner.Init()
	if err != nil {
		log.Error(err)
	}

	// let everyone know the job is running
	running(runner.client, runner.job, fmt.Sprintf("Job %s is running on host %s", runner.job.InvocationID, host))

	if err = runner.DockerLogin(); err != nil {
		log.Error(err)
	}

	composePath := cfg.GetString("docker-compose.path")
	pullCommand := exec.Command(composePath, "-f", "docker-compose.yml", "pull", "--parallel")
	pullCommand.Env = os.Environ()
	pullCommand.Dir = runner.workingDir
	pullCommand.Stdout = log.Writer()
	pullCommand.Stderr = log.Writer()
	err = pullCommand.Run()
	if err != nil {
		log.Error(err)
		runner.status = messaging.StatusDockerPullFailed
	}

	if err = fs.WriteJobSummary(fs.FS, runner.logsDir, job); err != nil {
		log.Error(err)
	}

	if err = fs.WriteJobParameters(fs.FS, runner.logsDir, job); err != nil {
		log.Error(err)
	}

	if runner.status == messaging.Success {
		if runner.status, err = runner.createDataContainers(); err != nil {
			log.Error(err)
		}
	}

	// If pulls didn't succeed then we can't guarantee that we've got the
	// correct versions of the tools. Don't bother pulling in data in that case,
	// things are already screwed up.
	if runner.status == messaging.Success {
		if runner.status, err = runner.downloadInputs(); err != nil {
			log.Error(err)
		}
	}
	// Only attempt to run the steps if the input downloads succeeded. No reason
	// to run the steps if there's no/corrupted data to operate on.
	if runner.status == messaging.Success {
		if runner.status, err = runner.runAllSteps(); err != nil {
			log.Error(err)
		}
	}
	// Always attempt to transfer outputs. There might be logs that can help
	// debug issues when the job fails.
	var outputStatus messaging.StatusCode
	running(runner.client, runner.job, fmt.Sprintf("Beginning to upload outputs to %s", runner.job.OutputDirectory()))
	if outputStatus, err = runner.uploadOutputs(); err != nil {
		log.Error(err)
	}
	if outputStatus != messaging.Success {
		runner.status = outputStatus
	}
	// Always inform upstream of the job status.
	if runner.status != messaging.Success {
		fail(runner.client, runner.job, fmt.Sprintf("Job exited with a status of %d", runner.status))
	} else {
		success(runner.client, runner.job)
	}
	exit <- runner.status
}
