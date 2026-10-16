// Package ide opens a task's worktree in a JetBrains IDE and writes a run
// configuration for the service it contains.
//
// There is deliberately no attempt to press Run: JetBrains exposes no way to
// trigger a run configuration in a running IDE from outside — the Toolbox
// launcher is `open -na <IDE> --args …`, and the IDE accepts a project path
// plus diff/merge/format/inspect, nothing else. So this gets it to one click.
//
// When a service is handed to the IDE, the stack must NOT also start it
// headlessly, or the two fight over the same port. That is the caller's job.
package ide

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Known IDEs, by the Toolbox launcher name.
const (
	IntelliJ = "idea"
	PyCharm  = "pycharm"
)

// toolboxScripts is where JetBrains Toolbox puts its launchers. It is on the
// user's interactive PATH but not necessarily on ours, so it is looked up
// explicitly rather than relying on the environment.
func toolboxScripts(home string) string {
	return filepath.Join(home, "Library", "Application Support", "JetBrains", "Toolbox", "scripts")
}

// Available reports which launchers exist, so the UI can offer only those.
func Available(home string) []string {
	var out []string
	for _, name := range []string{IntelliJ, PyCharm} {
		if _, err := os.Stat(filepath.Join(toolboxScripts(home), name)); err == nil {
			out = append(out, name)
			continue
		}
		if _, err := exec.LookPath(name); err == nil {
			out = append(out, name)
		}
	}
	return out
}

// Detect guesses the right IDE from what the project is built with.
func Detect(repoPath string) string {
	for _, f := range []string{"build.gradle.kts", "build.gradle", "pom.xml"} {
		if _, err := os.Stat(filepath.Join(repoPath, f)); err == nil {
			return IntelliJ
		}
	}
	for _, f := range []string{"pyproject.toml", "setup.py", "requirements.txt"} {
		if _, err := os.Stat(filepath.Join(repoPath, f)); err == nil {
			return PyCharm
		}
	}
	// A pure frontend project opens fine in IntelliJ Ultimate.
	if _, err := os.Stat(filepath.Join(repoPath, "package.json")); err == nil {
		return IntelliJ
	}
	return ""
}

// bundleNames maps a launcher to the application bundles it might be installed as.
var bundleNames = map[string][]string{
	IntelliJ: {"IntelliJ IDEA.app", "IntelliJ IDEA Ultimate.app", "IntelliJ IDEA Community Edition.app"},
	PyCharm:  {"PyCharm.app", "PyCharm Professional.app", "PyCharm Community Edition.app"},
}

// bundle finds the installed .app for a launcher.
func bundle(home, ideName string) string {
	for _, dir := range []string{filepath.Join(home, "Applications"), "/Applications"} {
		for _, n := range bundleNames[ideName] {
			p := filepath.Join(dir, n)
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return ""
}

// Open asks the IDE to open a directory.
//
// This uses `open -a <bundle>`, NOT the Toolbox launcher. The launcher runs
// `open -na …`, and the -n forces a SECOND instance of a single-instance app:
// it starts a whole new JVM which then has to negotiate with the running one
// over its lock socket, and if that one is busy or wedged the new process dies
// with a DirectoryLock stacktrace. `open -a` instead asks LaunchServices to
// hand the path to the instance already running, so there is nothing to race.
func Open(home, ideName, dir string) error {
	app := bundle(home, ideName)
	if app == "" {
		// Fall back to the Toolbox launcher when the bundle is somewhere unusual.
		bin := filepath.Join(toolboxScripts(home), ideName)
		if _, err := os.Stat(bin); err != nil {
			p, lookErr := exec.LookPath(ideName)
			if lookErr != nil {
				return fmt.Errorf("%s not found — install it, or add its command-line launcher from JetBrains Toolbox", ideName)
			}
			bin = p
		}
		cmd := exec.Command(bin, dir)
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("could not launch %s: %w", ideName, err)
		}
		go func() { _ = cmd.Wait() }()
		return nil
	}

	out, err := exec.Command("open", "-a", app, dir).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		if Running(home, ideName) {
			return fmt.Errorf("%s is already running but did not accept the project (%s) — "+
				"that instance may be busy or wedged; bring it to the front, and force-quit it if it does not respond",
				ideName, msg)
		}
		return fmt.Errorf("could not open %s: %s", ideName, msg)
	}
	return nil
}

// Running reports whether the IDE's main process is up, so a failure to open can
// distinguish "not installed" from "running but not responding".
func Running(home, ideName string) bool {
	app := bundle(home, ideName)
	if app == "" {
		return false
	}
	out, err := exec.Command("pgrep", "-f", filepath.Join(app, "Contents", "MacOS")).Output()
	return err == nil && strings.TrimSpace(string(out)) != ""
}

// --- run configurations ------------------------------------------------------

// RunConfig describes what to generate for one service.
type RunConfig struct {
	Name string // shown in the IDE's run-configuration dropdown
	XML  string
	// Note is surfaced to the user when the configuration is deliberately
	// incomplete — currently only for secrets we refuse to duplicate on disk.
	Note string
}

func esc(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

// Gradle builds an IntelliJ Gradle run configuration.
//
// No port is injected here: a service that reads its port from a local config
// file (the profile's `files`) has already had this task's port written there
// by the stack — so the IDE picks up the same instance port automatically.
func Gradle(name, task, scriptParams string) RunConfig {
	return RunConfig{
		Name: name,
		XML: `<component name="ProjectRunConfigurationManager">
  <configuration default="false" name="` + esc(name) + `" type="GradleRunConfiguration" factoryName="Gradle">
    <ExternalSystemSettings>
      <option name="executionName" />
      <option name="externalProjectPath" value="$PROJECT_DIR$" />
      <option name="externalSystemIdString" value="GRADLE" />
      <option name="scriptParameters" value="` + esc(scriptParams) + `" />
      <option name="taskDescriptions">
        <list />
      </option>
      <option name="taskNames">
        <list>
          <option value="` + esc(task) + `" />
        </list>
      </option>
      <option name="vmOptions" />
    </ExternalSystemSettings>
    <ExternalSystemDebugServerProcess>true</ExternalSystemDebugServerProcess>
    <ExternalSystemReattachDebugProcess>true</ExternalSystemReattachDebugProcess>
    <DebugAllEnabled>false</DebugAllEnabled>
    <method v="2" />
  </configuration>
</component>
`}
}

// NPM builds an IntelliJ npm run configuration.
func NPM(name, script string) RunConfig {
	return RunConfig{
		Name: name,
		XML: `<component name="ProjectRunConfigurationManager">
  <configuration default="false" name="` + esc(name) + `" type="js.build_tools.npm" nameIsGenerated="false">
    <package-json value="$PROJECT_DIR$/package.json" />
    <command value="run" />
    <scripts>
      <script value="` + esc(script) + `" />
    </scripts>
    <node-interpreter value="project" />
    <envs />
    <method v="2" />
  </configuration>
</component>
`}
}

// Uvicorn builds a PyCharm run configuration for a FastAPI service.
//
// Secret-bearing variables are deliberately NOT written: duplicating a token
// out of .env.properties into a second file on disk is exactly the kind of
// spread that makes secrets leak. They are listed in Note instead, to be set
// once in the IDE where they stay in the IDE's own storage.
func Uvicorn(name, module string, port int, env map[string]string, omitted []string) RunConfig {
	var envs strings.Builder
	envs.WriteString("    <envs>\n")
	for k, v := range env {
		envs.WriteString(`      <env name="` + esc(k) + `" value="` + esc(v) + `" />` + "\n")
	}
	envs.WriteString("    </envs>\n")

	note := ""
	if len(omitted) > 0 {
		note = "set " + strings.Join(omitted, ", ") + " in the run configuration once — " +
			"not written to disk here to avoid copying the secret out of .env.properties"
	}
	return RunConfig{
		Name: name,
		Note: note,
		XML: `<component name="ProjectRunConfigurationManager">
  <configuration default="false" name="` + esc(name) + `" type="PythonConfigurationType" factoryName="Python">
    <option name="INTERPRETER_OPTIONS" value="" />
    <option name="PARENT_ENVS" value="true" />
` + envs.String() + `    <option name="SDK_HOME" value="" />
    <option name="WORKING_DIRECTORY" value="$PROJECT_DIR$" />
    <option name="IS_MODULE_SDK" value="true" />
    <option name="ADD_CONTENT_ROOTS" value="true" />
    <option name="ADD_SOURCE_ROOTS" value="true" />
    <option name="SCRIPT_NAME" value="uvicorn" />
    <option name="PARAMETERS" value="` + esc(fmt.Sprintf("%s --host 0.0.0.0 --port %d", module, port)) + `" />
    <option name="MODULE_MODE" value="true" />
    <option name="REDIRECT_INPUT" value="false" />
    <option name="EMULATE_TERMINAL" value="false" />
    <option name="ADD_CONTENT_ROOTS" value="true" />
    <method v="2" />
  </configuration>
</component>
`}
}

// Write puts the configuration where the IDE will find it. .idea is gitignored
// in these repos, so this does not dirty the worktree.
func Write(worktree string, rc RunConfig) (string, error) {
	dir := filepath.Join(worktree, ".idea", "runConfigurations")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	// IntelliJ derives the file name from the configuration name; spaces and
	// punctuation become underscores.
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		default:
			return '_'
		}
	}, rc.Name)
	path := filepath.Join(dir, safe+".xml")
	return path, os.WriteFile(path, []byte(rc.XML), 0o644)
}
