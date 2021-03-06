package builder

// This file contains the dispatchers for each command. Note that
// `nullDispatch` is not actually a command, but support for commands we parse
// but do nothing with.
//
// See evaluator.go for a higher level discussion of the whole evaluator
// package.

import (
	"fmt"
	"io/ioutil"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Sirupsen/logrus"
	"github.com/docker/docker/nat"
	flag "github.com/docker/docker/pkg/mflag"
	"github.com/docker/docker/runconfig"
)

const (
	// NoBaseImageSpecifier is the symbol used by the FROM
	// command to specify that no base image is to be used.
	NoBaseImageSpecifier string = "scratch"
)

// dispatch with no layer / parsing. This is effectively not a command.
func nullDispatch(b *Builder, args []string, attributes map[string]bool, original string) error {
	return nil
}

// ENV foo bar
//
// Sets the environment variable foo to bar, also makes interpolation
// in the dockerfile available from the next statement on via ${foo}.
//
func env(b *Builder, args []string, attributes map[string]bool, original string) error {
	if len(args) == 0 {
		return fmt.Errorf("ENV requires at least one argument")
	}

	if len(args)%2 != 0 {
		// should never get here, but just in case
		return fmt.Errorf("Bad input to ENV, too many args")
	}

	commitStr := "ENV"

	for j := 0; j < len(args); j++ {
		// name  ==> args[j]
		// value ==> args[j+1]
		newVar := args[j] + "=" + args[j+1] + ""
		commitStr += " " + newVar

		gotOne := false
		for i, envVar := range b.Config.Env {
			envParts := strings.SplitN(envVar, "=", 2)
			if envParts[0] == args[j] {
				b.Config.Env[i] = newVar
				gotOne = true
				break
			}
		}
		if !gotOne {
			b.Config.Env = append(b.Config.Env, newVar)
		}
		j++
	}

	return b.commit("", b.Config.Cmd, commitStr)
}

// MAINTAINER some text <maybe@an.email.address>
//
// Sets the maintainer metadata.
func maintainer(b *Builder, args []string, attributes map[string]bool, original string) error {
	if len(args) != 1 {
		return fmt.Errorf("MAINTAINER requires exactly one argument")
	}

	b.maintainer = args[0]
	return b.commit("", b.Config.Cmd, fmt.Sprintf("MAINTAINER %s", b.maintainer))
}

// LABEL some json data describing the image
//
// Sets the Label variable foo to bar,
//
func label(b *Builder, args []string, attributes map[string]bool, original string) error {
	if len(args) == 0 {
		return fmt.Errorf("LABEL requires at least one argument")
	}
	if len(args)%2 != 0 {
		// should never get here, but just in case
		return fmt.Errorf("Bad input to LABEL, too many args")
	}

	commitStr := "LABEL"

	if b.Config.Labels == nil {
		b.Config.Labels = map[string]string{}
	}

	for j := 0; j < len(args); j++ {
		// name  ==> args[j]
		// value ==> args[j+1]
		newVar := args[j] + "=" + args[j+1] + ""
		commitStr += " " + newVar

		b.Config.Labels[args[j]] = args[j+1]
		j++
	}
	return b.commit("", b.Config.Cmd, commitStr)
}

// ADD foo /path
//
// Add the file 'foo' to '/path'. Tarball and Remote URL (git, http) handling
// exist here. If you do not wish to have this automatic handling, use COPY.
//
func add(b *Builder, args []string, attributes map[string]bool, original string) error {
	if len(args) < 2 {
		return fmt.Errorf("ADD requires at least two arguments")
	}

	return b.runContextCommand(args, true, true, "ADD")
}

// COPY foo /path
//
// Same as 'ADD' but without the tar and remote url handling.
//
func dispatchCopy(b *Builder, args []string, attributes map[string]bool, original string) error {
	if len(args) < 2 {
		return fmt.Errorf("COPY requires at least two arguments")
	}

	return b.runContextCommand(args, false, false, "COPY")
}

// FROM imagename
//
// This sets the image the dockerfile will build on top of.
//
func from(b *Builder, args []string, attributes map[string]bool, original string) error {
	if len(args) != 1 {
		return fmt.Errorf("FROM requires one argument")
	}

	name := args[0]

	if name == NoBaseImageSpecifier {
		b.image = ""
		b.noBaseImage = true
		return nil
	}

	image, err := b.Daemon.Repositories().LookupImage(name)
	if b.Pull {
		image, err = b.pullImage(name)
		if err != nil {
			return err
		}
	}
	if err != nil {
		if b.Daemon.Graph().IsNotExist(err, name) {
			image, err = b.pullImage(name)
		}

		// note that the top level err will still be !nil here if IsNotExist is
		// not the error. This approach just simplifies the logic a bit.
		if err != nil {
			return err
		}
	}

	return b.processImageFrom(image)
}

// ONBUILD RUN echo yo
//
// ONBUILD triggers run when the image is used in a FROM statement.
//
// ONBUILD handling has a lot of special-case functionality, the heading in
// evaluator.go and comments around dispatch() in the same file explain the
// special cases. search for 'OnBuild' in internals.go for additional special
// cases.
//
func onbuild(b *Builder, args []string, attributes map[string]bool, original string) error {
	if len(args) == 0 {
		return fmt.Errorf("ONBUILD requires at least one argument")
	}

	triggerInstruction := strings.ToUpper(strings.TrimSpace(args[0]))
	switch triggerInstruction {
	case "ONBUILD":
		return fmt.Errorf("Chaining ONBUILD via `ONBUILD ONBUILD` isn't allowed")
	case "MAINTAINER", "FROM":
		return fmt.Errorf("%s isn't allowed as an ONBUILD trigger", triggerInstruction)
	}

	original = regexp.MustCompile(`(?i)^\s*ONBUILD\s*`).ReplaceAllString(original, "")

	b.Config.OnBuild = append(b.Config.OnBuild, original)
	return b.commit("", b.Config.Cmd, fmt.Sprintf("ONBUILD %s", original))
}

// WORKDIR /tmp
//
// Set the working directory for future RUN/CMD/etc statements.
//
func workdir(b *Builder, args []string, attributes map[string]bool, original string) error {
	if len(args) != 1 {
		return fmt.Errorf("WORKDIR requires exactly one argument")
	}

	workdir := args[0]

	if !filepath.IsAbs(workdir) {
		workdir = filepath.Join("/", b.Config.WorkingDir, workdir)
	}

	b.Config.WorkingDir = workdir

	return b.commit("", b.Config.Cmd, fmt.Sprintf("WORKDIR %v", workdir))
}

// RUN some command yo
//
// run a command and commit the image. Args are automatically prepended with
// 'sh -c' in the event there is only one argument. The difference in
// processing:
//
// RUN echo hi          # sh -c echo hi
// RUN [ "echo", "hi" ] # echo hi
//
func run(b *Builder, args []string, attributes map[string]bool, original string) error {
	if b.image == "" && !b.noBaseImage {
		return fmt.Errorf("Please provide a source image with `from` prior to run")
	}

	args = handleJsonArgs(args, attributes)

	if !attributes["json"] {
		args = append([]string{"/bin/sh", "-c"}, args...)
	}

	runCmd := flag.NewFlagSet("run", flag.ContinueOnError)
	runCmd.SetOutput(ioutil.Discard)
	runCmd.Usage = nil

	config, _, _, err := runconfig.Parse(runCmd, append([]string{b.image}, args...))
	if err != nil {
		return err
	}

	cmd := b.Config.Cmd
	// set Cmd manually, this is special case only for Dockerfiles
	b.Config.Cmd = config.Cmd
	runconfig.Merge(b.Config, config)

	defer func(cmd []string) { b.Config.Cmd = cmd }(cmd)

	logrus.Debugf("[BUILDER] Command to be executed: %v", b.Config.Cmd)

	hit, err := b.probeCache()
	if err != nil {
		return err
	}
	if hit {
		return nil
	}

	c, err := b.create()
	if err != nil {
		return err
	}

	// Ensure that we keep the container mounted until the commit
	// to avoid unmounting and then mounting directly again
	c.Mount()
	defer c.Unmount()

	err = b.run(c)
	if err != nil {
		return err
	}
	if err := b.commit(c.ID, cmd, "run"); err != nil {
		return err
	}

	return nil
}

// CMD foo
//
// Set the default command to run in the container (which may be empty).
// Argument handling is the same as RUN.
//
func cmd(b *Builder, args []string, attributes map[string]bool, original string) error {
	b.Config.Cmd = handleJsonArgs(args, attributes)

	if !attributes["json"] {
		b.Config.Cmd = append([]string{"/bin/sh", "-c"}, b.Config.Cmd...)
	}

	if err := b.commit("", b.Config.Cmd, fmt.Sprintf("CMD %q", b.Config.Cmd)); err != nil {
		return err
	}

	if len(args) != 0 {
		b.cmdSet = true
	}

	return nil
}

// ENTRYPOINT /usr/sbin/nginx
//
// Set the entrypoint (which defaults to sh -c) to /usr/sbin/nginx. Will
// accept the CMD as the arguments to /usr/sbin/nginx.
//
// Handles command processing similar to CMD and RUN, only b.Config.Entrypoint
// is initialized at NewBuilder time instead of through argument parsing.
//
func entrypoint(b *Builder, args []string, attributes map[string]bool, original string) error {
	parsed := handleJsonArgs(args, attributes)

	switch {
	case attributes["json"]:
		// ENTRYPOINT ["echo", "hi"]
		b.Config.Entrypoint = parsed
	case len(parsed) == 0:
		// ENTRYPOINT []
		b.Config.Entrypoint = nil
	default:
		// ENTRYPOINT echo hi
		b.Config.Entrypoint = []string{"/bin/sh", "-c", parsed[0]}
	}

	// when setting the entrypoint if a CMD was not explicitly set then
	// set the command to nil
	if !b.cmdSet {
		b.Config.Cmd = nil
	}

	if err := b.commit("", b.Config.Cmd, fmt.Sprintf("ENTRYPOINT %q", b.Config.Entrypoint)); err != nil {
		return err
	}

	return nil
}

// EXPOSE 6667/tcp 7000/tcp
//
// Expose ports for links and port mappings. This all ends up in
// b.Config.ExposedPorts for runconfig.
//
func expose(b *Builder, args []string, attributes map[string]bool, original string) error {
	portsTab := args

	if len(args) == 0 {
		return fmt.Errorf("EXPOSE requires at least one argument")
	}

	if b.Config.ExposedPorts == nil {
		b.Config.ExposedPorts = make(nat.PortSet)
	}

	ports, bindingMap, err := nat.ParsePortSpecs(append(portsTab, b.Config.PortSpecs...))
	if err != nil {
		return err
	}

	for _, bindings := range bindingMap {
		if bindings[0].HostIp != "" || bindings[0].HostPort != "" {
			fmt.Fprintf(b.ErrStream, " ---> Using Dockerfile's EXPOSE instruction"+
				"      to map host ports to container ports (ip:hostPort:containerPort) is deprecated.\n"+
				"      Please use -p to publish the ports.\n")
		}
	}

	// instead of using ports directly, we build a list of ports and sort it so
	// the order is consistent. This prevents cache burst where map ordering
	// changes between builds
	portList := make([]string, len(ports))
	var i int
	for port := range ports {
		if _, exists := b.Config.ExposedPorts[port]; !exists {
			b.Config.ExposedPorts[port] = struct{}{}
		}
		portList[i] = string(port)
		i++
	}
	sort.Strings(portList)
	b.Config.PortSpecs = nil
	return b.commit("", b.Config.Cmd, fmt.Sprintf("EXPOSE %s", strings.Join(portList, " ")))
}

// USER foo
//
// Set the user to 'foo' for future commands and when running the
// ENTRYPOINT/CMD at container run time.
//
func user(b *Builder, args []string, attributes map[string]bool, original string) error {
	if len(args) != 1 {
		return fmt.Errorf("USER requires exactly one argument")
	}

	b.Config.User = args[0]
	return b.commit("", b.Config.Cmd, fmt.Sprintf("USER %v", args))
}

// VOLUME /foo
//
// Expose the volume /foo for use. Will also accept the JSON array form.
//
func volume(b *Builder, args []string, attributes map[string]bool, original string) error {
	if len(args) == 0 {
		return fmt.Errorf("VOLUME requires at least one argument")
	}

	if b.Config.Volumes == nil {
		b.Config.Volumes = map[string]struct{}{}
	}
	for _, v := range args {
		v = strings.TrimSpace(v)
		if v == "" {
			return fmt.Errorf("Volume specified can not be an empty string")
		}
		b.Config.Volumes[v] = struct{}{}
	}
	if err := b.commit("", b.Config.Cmd, fmt.Sprintf("VOLUME %v", args)); err != nil {
		return err
	}
	return nil
}

// INSERT is no longer accepted, but we still parse it.
func insert(b *Builder, args []string, attributes map[string]bool, original string) error {
	return fmt.Errorf("INSERT has been deprecated. Please use ADD instead")
}
