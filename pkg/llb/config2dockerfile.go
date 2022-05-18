package llb

import (
	"fmt"
	"gitlab.com/cmdjulian/mopy/pkg/config"
	"gitlab.com/cmdjulian/mopy/pkg/utils"
	"runtime"
	"strings"
)

const pipCacheMount = "--mount=type=cache,target=/root/.cache"
const aptCacheMount = "--mount=type=cache,target=/var/cache/apt --mount=type=cache,target=/var/lib/apt"

var defaultEnvs = map[string]string{
	"PIP_DISABLE_PIP_VERSION_CHECK": "1",
	"PIP_NO_WARN_SCRIPT_LOCATION":   "0",
	"PIP_USER":                      "1",
	"PYTHONPYCACHEPREFIX":           "\"$HOME/.pycache\"",
	"GIT_SSH_COMMAND":               "\"ssh -o StrictHostKeyChecking=no\"",
}

var defaulLabels = map[string]string{
	"org.opencontainers.image.description": "autogenerated by mopy",
	"moby.buildkit.frontend":               "mopy",
	"mopy.version":                         "v1",
}

func Mopyfile2LLB(c *config.Config) string {
	dockerfile := buildStage(c)
	dockerfile += runStage(c)

	return dockerfile
}

func buildStage(c *config.Config) string {
	dockerfile := from(c)
	dockerfile += apt(c)
	dockerfile += env(utils.Union(defaultEnvs, c.Envs))
	dockerfile += installDeps(c)
	dockerfile += clearCachedDataFromInstall(c)

	return dockerfile
}

// Determine flags like mount, ssh and cache
// transform local deps into relative paths, requirements.txt and ssh
// install all at one (local, requirements.txt, ssh, http and pypi)
func installDeps(c *config.Config) string {
	if len(c.PipDependencies) == 0 {
		return ""
	}

	flags := flags(c)
	args := args(c)

	COPY := ""
	RUN := fmt.Sprintf("\nRUN %s pip install %s", flags, args)

	for i, s := range c.LocalDependencies() {
		if !strings.HasSuffix(s, "/requirements.txt") {
			s = strings.TrimSuffix(s, "/")
			source := strings.TrimPrefix(s, "./")
			s = utils.After(s, "/") + "/"
			target := fmt.Sprintf("/tmp/%d%s", i, s)
			// should be supported with buildkit but isn't
			COPY += fmt.Sprintf("\nCOPY --link %s %s", source, target)
		}
	}

	return COPY + RUN
}

func args(c *config.Config) string {
	args := ""

	for i, s := range c.LocalDependencies() {
		if strings.HasSuffix(s, "/requirements.txt") {
			target := fmt.Sprintf("/tmp/%drequirements.txt", i)
			args += fmt.Sprintf("-r %s ", target)
		} else {
			s = strings.TrimSuffix(s, "/")
			s = utils.After(s, "/") + "/"
			target := fmt.Sprintf("/tmp/%d%s", i, s)
			args += fmt.Sprintf("%s ", target)
		}
	}

	deps := append(append(c.PyPiDependencies(), c.HttpDependencies()...), c.SshDependencies()...)
	if len(deps) > 0 {
		depString := strings.Join(deps, " ")
		args += fmt.Sprintf("%s ", depString)
	}

	return args
}

func flags(c *config.Config) string {
	flags := pipCacheMount

	if len(c.SshDependencies()) > 0 {
		flags += " --mount=type=ssh,required=true"
	}

	for i, s := range c.LocalDependencies() {
		if strings.HasSuffix(s, "/requirements.txt") {
			target := fmt.Sprintf("/tmp/%drequirements.txt", i)
			flags += fmt.Sprintf(" --mount=type=bind,source=%s,target=%s", s, target)
		}
	}

	return flags
}

func from(c *config.Config) string {
	line := fmt.Sprintf("FROM python:%s AS builder\n", c.PythonVersion)
	line += "RUN mkdir /build\n"
	line += "WORKDIR /build\n"

	return line
}

func apt(c *config.Config) string {
	line := "\n"

	if len(c.HttpDependencies()) > 0 || len(c.SshDependencies()) > 0 {
		line += fmt.Sprintf("RUN %s apt update && apt install -y git-lfs", aptCacheMount)
	} else if len(c.Apt) > 0 {
		line += fmt.Sprintf("RUN %s apt update && apt install -y ", aptCacheMount)
	}

	for _, apt := range c.Apt {
		line += fmt.Sprintf(" %s", apt)
	}

	return line
}

func env(envs map[string]string) string {
	line := "\nENV"
	for key, value := range envs {
		line += fmt.Sprintf(" %s=%s", key, value)
	}

	return line
}

func clearCachedDataFromInstall(c *config.Config) string {
	line := "\n"
	if len(c.PipDependencies) > 0 {
		line += "RUN find /root/.local/lib/python*/ -name 'tests' -exec rm -r '{}' + && "
		line += "find /root/.local/lib/python*/site-packages/ -name '*.so' -exec sh -c 'file \"{}\" | grep -q \"not stripped\" && strip -s \"{}\"' \\; && "
		line += "find /root/.local/lib/python*/ -type f -name '*.pyc' -delete && "
		line += "find /root/.local/lib/python*/ -type d -name '__pycache__' -delete\n"
	}

	return line
}

func runStage(c *config.Config) string {
	line := "\n"
	line += determineFinalBaseImage(c)
	line += labels(c.PythonVersion)

	line += env(utils.Union(map[string]string{"PYTHONUNBUFFERED": "1"}, c.Envs))
	if len(c.PipDependencies) > 0 {
		line += "\nCOPY --from=builder --chown=nonroot:nonroot /root/.local/ /home/nonroot/.local/"
	}

	if c.Project != "" {
		line += project(c)
	}

	return line
}

func determineFinalBaseImage(c *config.Config) string {
	if strings.HasPrefix(c.PythonVersion, "3.9") {
		switch runtime.GOARCH {
		case "arm64", "amd64":
			return distroless39()
		}
	}

	return fallback(c)
}

func labels(pythonVersion string) string {
	line := "\nLABEL"
	labels := map[string]string{
		"mopy.python.version": pythonVersion,
	}

	for key, value := range utils.Union(defaulLabels, labels) {
		line += fmt.Sprintf(" %s=\"%s\"", key, value)
	}

	return line
}

func distroless39() string {
	return "FROM gcr.io/distroless/python3:nonroot@sha256:49aeb0efbe5c01375e6d747c138c87cf89c6aa4dc5daac955b9afb6aba4027e4"
}

func fallback(c *config.Config) string {
	line := fmt.Sprintf("FROM python:%s-slim\n", c.PythonVersion)
	line += "RUN useradd --uid=65532 --user-group --home-dir=/home/nonroot --create-home nonroot\n"
	line += "USER 65532:65532"

	return line
}

func project(c *config.Config) string {
	line := "\n"

	project := strings.TrimSuffix(c.Project, "/")
	source := "/home/nonroot/" + utils.After(project, "/")
	line += fmt.Sprintf("COPY --chown=nonroot:nonroot %s %s\n", c.Project, source)
	line += "ENTRYPOINT [ \"python\", \"-u\" ]\n"

	if strings.HasSuffix(c.Project, ".py") {
		line += "WORKDIR /home/nonroot\n"
		line += fmt.Sprintf("CMD [ \"%s\" ]", source)
	} else {
		line += fmt.Sprintf("WORKDIR %s\n", source)
		line += "CMD [ \"main.py\" ]"
	}

	return line
}
