package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/docker/docker/archive"
	"github.com/docker/docker/engine"
	"github.com/docker/docker/nat"
	"github.com/docker/docker/pkg/log"
	"github.com/docker/docker/pkg/parsers"
	"github.com/docker/docker/pkg/symlink"
	"github.com/docker/docker/pkg/system"
	"github.com/docker/docker/pkg/tarsum"
	"github.com/docker/docker/registry"
	"github.com/docker/docker/runconfig"
	"github.com/docker/docker/utils"
)

func (daemon *Daemon) CmdBuild(job *engine.Job) engine.Status {
	if len(job.Args) != 0 {
		return job.Errorf("Usage: %s\n", job.Name)
	}
	var (
		remoteURL      = job.Getenv("remote")
		repoName       = job.Getenv("t")
		suppressOutput = job.GetenvBool("q")
		noCache        = job.GetenvBool("nocache")
		rm             = job.GetenvBool("rm")
		forceRm        = job.GetenvBool("forcerm")
		authConfig     = &registry.AuthConfig{}
		configFile     = &registry.ConfigFile{}
		tag            string
		context        io.ReadCloser
	)
	job.GetenvJson("authConfig", authConfig)
	job.GetenvJson("configFile", configFile)
	repoName, tag = parsers.ParseRepositoryTag(repoName)

	if remoteURL == "" {
		context = ioutil.NopCloser(job.Stdin)
	} else if utils.IsGIT(remoteURL) {
		if !strings.HasPrefix(remoteURL, "git://") {
			remoteURL = "https://" + remoteURL
		}
		root, err := ioutil.TempDir("", "docker-build-git")
		if err != nil {
			return job.Error(err)
		}
		defer os.RemoveAll(root)

		if output, err := exec.Command("git", "clone", "--recursive", remoteURL, root).CombinedOutput(); err != nil {
			return job.Errorf("Error trying to use git: %s (%s)", err, output)
		}

		c, err := archive.Tar(root, archive.Uncompressed)
		if err != nil {
			return job.Error(err)
		}
		context = c
	} else if utils.IsURL(remoteURL) {
		f, err := utils.Download(remoteURL)
		if err != nil {
			return job.Error(err)
		}
		defer f.Body.Close()
		dockerFile, err := ioutil.ReadAll(f.Body)
		if err != nil {
			return job.Error(err)
		}
		c, err := archive.Generate("Dockerfile", string(dockerFile))
		if err != nil {
			return job.Error(err)
		}
		context = c
	}
	defer context.Close()

	sf := utils.NewStreamFormatter(job.GetenvBool("json"))
	b := NewBuildFile(daemon, daemon.eng,
		&utils.StdoutFormater{
			Writer:          job.Stdout,
			StreamFormatter: sf,
		},
		&utils.StderrFormater{
			Writer:          job.Stdout,
			StreamFormatter: sf,
		},
		!suppressOutput, !noCache, rm, forceRm, job.Stdout, sf, authConfig, configFile)
	id, err := b.Build(context)
	if err != nil {
		return job.Error(err)
	}
	if repoName != "" {
		daemon.Repositories().Set(repoName, tag, id, false)
	}
	return engine.StatusOK
}

var (
	ErrDockerfileEmpty = errors.New("Dockerfile cannot be empty")
)

type BuildFile interface {
	Build(io.Reader) (string, error)
	CmdFrom(string) error
	CmdRun(string) error
}

type buildFile struct {
	daemon *Daemon        //Job所属daemon
	eng    *engine.Engine //Job所属engine

	image      string            //基础镜像
	maintainer string            //Docker维护者
	config     *runconfig.Config //运行配置参数

	contextPath string         //context所在路径
	context     *tarsum.TarSum //context内容

	verbose      bool //输出build
	utilizeCache bool //使用镜像缓存
	rm           bool //删除中间容器
	forceRm      bool //强制删除中间容器

	authConfig *registry.AuthConfig //认证信息
	configFile *registry.ConfigFile //配置文件

	tmpContainers map[string]struct{} //临时容器列表
	tmpImages     map[string]struct{} //临时镜像列表

	outStream io.Writer //输出流
	errStream io.Writer //输入流

	// Deprecated, original writer used for ImagePull. To be removed.
	outOld io.Writer
	sf     *utils.StreamFormatter

	// cmdSet indicates is CMD was set in current Dockerfile
	cmdSet bool
}

func (b *buildFile) clearTmp(containers map[string]struct{}) {
	for c := range containers {
		tmp := b.daemon.Get(c)
		if err := b.daemon.Destroy(tmp); err != nil {
			fmt.Fprintf(b.outStream, "Error removing intermediate container %s: %s\n", utils.TruncateID(c), err.Error())
		} else {
			delete(containers, c)
			fmt.Fprintf(b.outStream, "Removing intermediate container %s\n", utils.TruncateID(c))
		}
	}
}

func (b *buildFile) CmdFrom(name string) error {
	image, err := b.daemon.Repositories().LookupImage(name)
	if err != nil {
		if b.daemon.Graph().IsNotExist(err) {
			remote, tag := parsers.ParseRepositoryTag(name)
			pullRegistryAuth := b.authConfig
			if len(b.configFile.Configs) > 0 {
				// The request came with a full auth config file, we prefer to use that
				endpoint, _, err := registry.ResolveRepositoryName(remote)
				if err != nil {
					return err
				}
				resolvedAuth := b.configFile.ResolveAuthConfig(endpoint)
				pullRegistryAuth = &resolvedAuth
			}
			job := b.eng.Job("pull", remote, tag)
			job.SetenvBool("json", b.sf.Json())
			job.SetenvBool("parallel", true)
			job.SetenvJson("authConfig", pullRegistryAuth)
			job.Stdout.Add(b.outOld)
			if err := job.Run(); err != nil {
				return err
			}
			image, err = b.daemon.Repositories().LookupImage(name)
			if err != nil {
				return err
			}
		} else {
			return err
		}
	}
	b.image = image.ID
	b.config = &runconfig.Config{}
	if image.Config != nil {
		b.config = image.Config
	}
	if b.config.Env == nil || len(b.config.Env) == 0 {
		b.config.Env = append(b.config.Env, "PATH="+DefaultPathEnv)
	}
	// Process ONBUILD triggers if they exist
	if nTriggers := len(b.config.OnBuild); nTriggers != 0 {
		fmt.Fprintf(b.errStream, "# Executing %d build triggers\n", nTriggers)
	}

	// Copy the ONBUILD triggers, and remove them from the config, since the config will be commited.
	onBuildTriggers := b.config.OnBuild
	b.config.OnBuild = []string{}

	for n, step := range onBuildTriggers {
		splitStep := strings.Split(step, " ")
		stepInstruction := strings.ToUpper(strings.Trim(splitStep[0], " "))
		switch stepInstruction {
		case "ONBUILD":
			return fmt.Errorf("Source image contains forbidden chained `ONBUILD ONBUILD` trigger: %s", step)
		case "MAINTAINER", "FROM":
			return fmt.Errorf("Source image contains forbidden %s trigger: %s", stepInstruction, step)
		}
		if err := b.BuildStep(fmt.Sprintf("onbuild-%d", n), step); err != nil {
			return err
		}
	}
	return nil
}

// The ONBUILD command declares a build instruction to be executed in any future build
// using the current image as a base.
func (b *buildFile) CmdOnbuild(trigger string) error {
	splitTrigger := strings.Split(trigger, " ")
	triggerInstruction := strings.ToUpper(strings.Trim(splitTrigger[0], " "))
	switch triggerInstruction {
	case "ONBUILD":
		return fmt.Errorf("Chaining ONBUILD via `ONBUILD ONBUILD` isn't allowed")
	case "MAINTAINER", "FROM":
		return fmt.Errorf("%s isn't allowed as an ONBUILD trigger", triggerInstruction)
	}
	b.config.OnBuild = append(b.config.OnBuild, trigger)
	return b.commit("", b.config.Cmd, fmt.Sprintf("ONBUILD %s", trigger))
}

func (b *buildFile) CmdMaintainer(name string) error {
	b.maintainer = name
	return b.commit("", b.config.Cmd, fmt.Sprintf("MAINTAINER %s", name))
}

// probeCache checks to see if image-caching is enabled (`b.utilizeCache`)
// and if so attempts to look up the current `b.image` and `b.config` pair
// in the current server `b.daemon`. If an image is found, probeCache returns
// `(true, nil)`. If no image is found, it returns `(false, nil)`. If there
// is any error, it returns `(false, err)`.
func (b *buildFile) probeCache() (bool, error) {
	if b.utilizeCache {
		if cache, err := b.daemon.ImageGetCached(b.image, b.config); err != nil {
			return false, err
		} else if cache != nil {
			fmt.Fprintf(b.outStream, " ---> Using cache\n")
			log.Debugf("[BUILDER] Use cached version")
			b.image = cache.ID
			return true, nil
		} else {
			log.Debugf("[BUILDER] Cache miss")
		}
	}
	return false, nil
}

func (b *buildFile) CmdRun(args string) error {
	if b.image == "" {
		return fmt.Errorf("Please provide a source image with `from` prior to run")
	}
	config, _, _, err := runconfig.Parse(append([]string{b.image}, b.buildCmdFromJson(args)...), nil)
	if err != nil {
		return err
	}

	cmd := b.config.Cmd
	// set Cmd manually, this is special case only for Dockerfiles
	b.config.Cmd = config.Cmd
	runconfig.Merge(b.config, config)

	defer func(cmd []string) { b.config.Cmd = cmd }(cmd)

	log.Debugf("Command to be executed: %v", b.config.Cmd)

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

func (b *buildFile) FindEnvKey(key string) int {
	for k, envVar := range b.config.Env {
		envParts := strings.SplitN(envVar, "=", 2)
		if key == envParts[0] {
			return k
		}
	}
	return -1
}

func (b *buildFile) ReplaceEnvMatches(value string) (string, error) {
	exp, err := regexp.Compile("(\\\\\\\\+|[^\\\\]|\\b|\\A)\\$({?)([[:alnum:]_]+)(}?)")
	if err != nil {
		return value, err
	}
	matches := exp.FindAllString(value, -1)
	for _, match := range matches {
		match = match[strings.Index(match, "$"):]
		matchKey := strings.Trim(match, "${}")

		for _, envVar := range b.config.Env {
			envParts := strings.SplitN(envVar, "=", 2)
			envKey := envParts[0]
			envValue := envParts[1]

			if envKey == matchKey {
				value = strings.Replace(value, match, envValue, -1)
				break
			}
		}
	}
	return value, nil
}

func (b *buildFile) CmdEnv(args string) error {
	tmp := strings.SplitN(args, " ", 2)
	if len(tmp) != 2 {
		return fmt.Errorf("Invalid ENV format")
	}
	key := strings.Trim(tmp[0], " \t")
	value := strings.Trim(tmp[1], " \t")

	envKey := b.FindEnvKey(key)
	replacedValue, err := b.ReplaceEnvMatches(value)
	if err != nil {
		return err
	}
	replacedVar := fmt.Sprintf("%s=%s", key, replacedValue)

	if envKey >= 0 {
		b.config.Env[envKey] = replacedVar
	} else {
		b.config.Env = append(b.config.Env, replacedVar)
	}
	return b.commit("", b.config.Cmd, fmt.Sprintf("ENV %s", replacedVar))
}

func (b *buildFile) buildCmdFromJson(args string) []string {
	var cmd []string
	if err := json.Unmarshal([]byte(args), &cmd); err != nil {
		log.Debugf("Error unmarshalling: %s, setting to /bin/sh -c", err)
		cmd = []string{"/bin/sh", "-c", args}
	}
	return cmd
}

func (b *buildFile) CmdCmd(args string) error {
	cmd := b.buildCmdFromJson(args)
	b.config.Cmd = cmd
	if err := b.commit("", b.config.Cmd, fmt.Sprintf("CMD %v", cmd)); err != nil {
		return err
	}
	b.cmdSet = true
	return nil
}

func (b *buildFile) CmdEntrypoint(args string) error {
	entrypoint := b.buildCmdFromJson(args)
	b.config.Entrypoint = entrypoint
	// if there is no cmd in current Dockerfile - cleanup cmd
	if !b.cmdSet {
		b.config.Cmd = nil
	}
	if err := b.commit("", b.config.Cmd, fmt.Sprintf("ENTRYPOINT %v", entrypoint)); err != nil {
		return err
	}
	return nil
}

func (b *buildFile) CmdExpose(args string) error {
	portsTab := strings.Split(args, " ")

	if b.config.ExposedPorts == nil {
		b.config.ExposedPorts = make(nat.PortSet)
	}
	ports, _, err := nat.ParsePortSpecs(append(portsTab, b.config.PortSpecs...))
	if err != nil {
		return err
	}
	for port := range ports {
		if _, exists := b.config.ExposedPorts[port]; !exists {
			b.config.ExposedPorts[port] = struct{}{}
		}
	}
	b.config.PortSpecs = nil

	return b.commit("", b.config.Cmd, fmt.Sprintf("EXPOSE %v", ports))
}

func (b *buildFile) CmdUser(args string) error {
	b.config.User = args
	return b.commit("", b.config.Cmd, fmt.Sprintf("USER %v", args))
}

func (b *buildFile) CmdInsert(args string) error {
	return fmt.Errorf("INSERT has been deprecated. Please use ADD instead")
}

func (b *buildFile) CmdCopy(args string) error {
	return b.runContextCommand(args, false, false, "COPY")
}

func (b *buildFile) CmdWorkdir(workdir string) error {
	if workdir[0] == '/' {
		b.config.WorkingDir = workdir
	} else {
		if b.config.WorkingDir == "" {
			b.config.WorkingDir = "/"
		}
		b.config.WorkingDir = filepath.Join(b.config.WorkingDir, workdir)
	}
	return b.commit("", b.config.Cmd, fmt.Sprintf("WORKDIR %v", workdir))
}

func (b *buildFile) CmdVolume(args string) error {
	if args == "" {
		return fmt.Errorf("Volume cannot be empty")
	}

	var volume []string
	if err := json.Unmarshal([]byte(args), &volume); err != nil {
		volume = []string{args}
	}
	if b.config.Volumes == nil {
		b.config.Volumes = map[string]struct{}{}
	}
	for _, v := range volume {
		b.config.Volumes[v] = struct{}{}
	}
	if err := b.commit("", b.config.Cmd, fmt.Sprintf("VOLUME %s", args)); err != nil {
		return err
	}
	return nil
}

func (b *buildFile) checkPathForAddition(orig string) error {
	origPath := path.Join(b.contextPath, orig)
	if p, err := filepath.EvalSymlinks(origPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%s: no such file or directory", orig)
		}
		return err
	} else {
		origPath = p
	}
	if !strings.HasPrefix(origPath, b.contextPath) {
		return fmt.Errorf("Forbidden path outside the build context: %s (%s)", orig, origPath)
	}
	_, err := os.Stat(origPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%s: no such file or directory", orig)
		}
		return err
	}
	return nil
}

func (b *buildFile) addContext(container *Container, orig, dest string, decompress bool) error {
	var (
		err        error
		destExists = true
		origPath   = path.Join(b.contextPath, orig)
		destPath   = path.Join(container.RootfsPath(), dest)
	)

	if destPath != container.RootfsPath() {
		destPath, err = symlink.FollowSymlinkInScope(destPath, container.RootfsPath())
		if err != nil {
			return err
		}
	}

	// Preserve the trailing '/'
	if strings.HasSuffix(dest, "/") || dest == "." {
		destPath = destPath + "/"
	}

	destStat, err := os.Stat(destPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		destExists = false
	}

	fi, err := os.Stat(origPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%s: no such file or directory", orig)
		}
		return err
	}

	if fi.IsDir() {
		return copyAsDirectory(origPath, destPath, destExists)
	}

	// If we are adding a remote file (or we've been told not to decompress), do not try to untar it
	if decompress {
		// First try to unpack the source as an archive
		// to support the untar feature we need to clean up the path a little bit
		// because tar is very forgiving.  First we need to strip off the archive's
		// filename from the path but this is only added if it does not end in / .
		tarDest := destPath
		if strings.HasSuffix(tarDest, "/") {
			tarDest = filepath.Dir(destPath)
		}

		// try to successfully untar the orig
		if err := archive.UntarPath(origPath, tarDest); err == nil {
			return nil
		} else if err != io.EOF {
			log.Debugf("Couldn't untar %s to %s: %s", origPath, tarDest, err)
		}
	}

	if err := os.MkdirAll(path.Dir(destPath), 0755); err != nil {
		return err
	}
	if err := archive.CopyWithTar(origPath, destPath); err != nil {
		return err
	}

	resPath := destPath
	if destExists && destStat.IsDir() {
		resPath = path.Join(destPath, path.Base(origPath))
	}

	return fixPermissions(resPath, 0, 0)
}

func (b *buildFile) runContextCommand(args string, allowRemote bool, allowDecompression bool, cmdName string) error {
	if b.context == nil {
		return fmt.Errorf("No context given. Impossible to use %s", cmdName)
	}
	tmp := strings.SplitN(args, " ", 2)
	if len(tmp) != 2 {
		return fmt.Errorf("Invalid %s format", cmdName)
	}

	orig, err := b.ReplaceEnvMatches(strings.Trim(tmp[0], " \t"))
	if err != nil {
		return err
	}

	dest, err := b.ReplaceEnvMatches(strings.Trim(tmp[1], " \t"))
	if err != nil {
		return err
	}

	cmd := b.config.Cmd
	b.config.Cmd = []string{"/bin/sh", "-c", fmt.Sprintf("#(nop) %s %s in %s", cmdName, orig, dest)}
	defer func(cmd []string) { b.config.Cmd = cmd }(cmd)
	b.config.Image = b.image

	var (
		origPath   = orig
		destPath   = dest
		remoteHash string
		isRemote   bool
		decompress = true
	)

	isRemote = utils.IsURL(orig)
	if isRemote && !allowRemote {
		return fmt.Errorf("Source can't be an URL for %s", cmdName)
	} else if utils.IsURL(orig) {
		// Initiate the download
		resp, err := utils.Download(orig)
		if err != nil {
			return err
		}

		// Create a tmp dir
		tmpDirName, err := ioutil.TempDir(b.contextPath, "docker-remote")
		if err != nil {
			return err
		}

		// Create a tmp file within our tmp dir
		tmpFileName := path.Join(tmpDirName, "tmp")
		tmpFile, err := os.OpenFile(tmpFileName, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmpDirName)

		// Download and dump result to tmp file
		if _, err := io.Copy(tmpFile, resp.Body); err != nil {
			tmpFile.Close()
			return err
		}
		tmpFile.Close()

		// Remove the mtime of the newly created tmp file
		if err := system.UtimesNano(tmpFileName, make([]syscall.Timespec, 2)); err != nil {
			return err
		}

		origPath = path.Join(filepath.Base(tmpDirName), filepath.Base(tmpFileName))

		// Process the checksum
		r, err := archive.Tar(tmpFileName, archive.Uncompressed)
		if err != nil {
			return err
		}
		tarSum := &tarsum.TarSum{Reader: r, DisableCompression: true}
		if _, err := io.Copy(ioutil.Discard, tarSum); err != nil {
			return err
		}
		remoteHash = tarSum.Sum(nil)
		r.Close()

		// If the destination is a directory, figure out the filename.
		if strings.HasSuffix(dest, "/") {
			u, err := url.Parse(orig)
			if err != nil {
				return err
			}
			path := u.Path
			if strings.HasSuffix(path, "/") {
				path = path[:len(path)-1]
			}
			parts := strings.Split(path, "/")
			filename := parts[len(parts)-1]
			if filename == "" {
				return fmt.Errorf("cannot determine filename from url: %s", u)
			}
			destPath = dest + filename
		}
	}

	if err := b.checkPathForAddition(origPath); err != nil {
		return err
	}

	// Hash path and check the cache
	if b.utilizeCache {
		var (
			hash string
			sums = b.context.GetSums()
		)

		if remoteHash != "" {
			hash = remoteHash
		} else if fi, err := os.Stat(path.Join(b.contextPath, origPath)); err != nil {
			return err
		} else if fi.IsDir() {
			var subfiles []string
			for file, sum := range sums {
				absFile := path.Join(b.contextPath, file)
				absOrigPath := path.Join(b.contextPath, origPath)
				if strings.HasPrefix(absFile, absOrigPath) {
					subfiles = append(subfiles, sum)
				}
			}
			sort.Strings(subfiles)
			hasher := sha256.New()
			hasher.Write([]byte(strings.Join(subfiles, ",")))
			hash = "dir:" + hex.EncodeToString(hasher.Sum(nil))
		} else {
			if origPath[0] == '/' && len(origPath) > 1 {
				origPath = origPath[1:]
			}
			origPath = strings.TrimPrefix(origPath, "./")
			if h, ok := sums[origPath]; ok {
				hash = "file:" + h
			}
		}
		b.config.Cmd = []string{"/bin/sh", "-c", fmt.Sprintf("#(nop) %s %s in %s", cmdName, hash, dest)}
		hit, err := b.probeCache()
		if err != nil {
			return err
		}
		// If we do not have a hash, never use the cache
		if hit && hash != "" {
			return nil
		}
	}

	// Create the container
	container, _, err := b.daemon.Create(b.config, "")
	if err != nil {
		return err
	}
	b.tmpContainers[container.ID] = struct{}{}

	if err := container.Mount(); err != nil {
		return err
	}
	defer container.Unmount()

	if !allowDecompression || isRemote {
		decompress = false
	}
	if err := b.addContext(container, origPath, destPath, decompress); err != nil {
		return err
	}

	if err := b.commit(container.ID, cmd, fmt.Sprintf("%s %s in %s", cmdName, orig, dest)); err != nil {
		return err
	}
	return nil
}

func (b *buildFile) CmdAdd(args string) error {
	return b.runContextCommand(args, true, true, "ADD")
}

func (b *buildFile) create() (*Container, error) {
	if b.image == "" {
		return nil, fmt.Errorf("Please provide a source image with `from` prior to run")
	}
	b.config.Image = b.image

	// Create the container
	c, _, err := b.daemon.Create(b.config, "")
	if err != nil {
		return nil, err
	}
	b.tmpContainers[c.ID] = struct{}{}
	fmt.Fprintf(b.outStream, " ---> Running in %s\n", utils.TruncateID(c.ID))

	// override the entry point that may have been picked up from the base image
	c.Path = b.config.Cmd[0]
	c.Args = b.config.Cmd[1:]

	return c, nil
}

func (b *buildFile) run(c *Container) error {
	var errCh chan error
	if b.verbose {
		errCh = utils.Go(func() error {
			// FIXME: call the 'attach' job so that daemon.Attach can be made private
			//
			// FIXME (LK4D4): Also, maybe makes sense to call "logs" job, it is like attach
			// but without hijacking for stdin. Also, with attach there can be race
			// condition because of some output already was printed before it.
			return <-b.daemon.Attach(c, nil, nil, b.outStream, b.errStream)
		})
	}

	//start the container
	if err := c.Start(); err != nil {
		return err
	}

	if errCh != nil {
		if err := <-errCh; err != nil {
			return err
		}
	}

	// Wait for it to finish
	if ret, _ := c.State.WaitStop(-1 * time.Second); ret != 0 {
		err := &utils.JSONError{
			Message: fmt.Sprintf("The command %v returned a non-zero code: %d", b.config.Cmd, ret),
			Code:    ret,
		}
		return err
	}

	return nil
}

// Commit the container <id> with the autorun command <autoCmd>
func (b *buildFile) commit(id string, autoCmd []string, comment string) error {
	if b.image == "" {
		return fmt.Errorf("Please provide a source image with `from` prior to commit")
	}
	b.config.Image = b.image
	if id == "" {
		cmd := b.config.Cmd
		b.config.Cmd = []string{"/bin/sh", "-c", "#(nop) " + comment}
		defer func(cmd []string) { b.config.Cmd = cmd }(cmd)

		hit, err := b.probeCache()
		if err != nil {
			return err
		}
		if hit {
			return nil
		}

		container, warnings, err := b.daemon.Create(b.config, "")
		if err != nil {
			return err
		}
		for _, warning := range warnings {
			fmt.Fprintf(b.outStream, " ---> [Warning] %s\n", warning)
		}
		b.tmpContainers[container.ID] = struct{}{}
		fmt.Fprintf(b.outStream, " ---> Running in %s\n", utils.TruncateID(container.ID))
		id = container.ID

		if err := container.Mount(); err != nil {
			return err
		}
		defer container.Unmount()
	}
	container := b.daemon.Get(id)
	if container == nil {
		return fmt.Errorf("An error occured while creating the container")
	}

	// Note: Actually copy the struct
	autoConfig := *b.config
	autoConfig.Cmd = autoCmd
	// Commit the container
	image, err := b.daemon.Commit(container, "", "", "", b.maintainer, true, &autoConfig)
	if err != nil {
		return err
	}
	b.tmpImages[image.ID] = struct{}{}
	b.image = image.ID
	return nil
}

// Long lines can be split with a backslash
var lineContinuation = regexp.MustCompile(`\\\s*\n`)

func (b *buildFile) Build(context io.Reader) (string, error) {
	tmpdirPath, err := ioutil.TempDir("", "docker-build")
	if err != nil {
		return "", err
	}

	decompressedStream, err := archive.DecompressStream(context)
	if err != nil {
		return "", err
	}

	b.context = &tarsum.TarSum{Reader: decompressedStream, DisableCompression: true}
	if err := archive.Untar(b.context, tmpdirPath, nil); err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpdirPath)

	b.contextPath = tmpdirPath
	filename := path.Join(tmpdirPath, "Dockerfile")
	if _, err := os.Stat(filename); os.IsNotExist(err) {
		return "", fmt.Errorf("Can't build a directory with no Dockerfile")
	}
	fileBytes, err := ioutil.ReadFile(filename)
	if err != nil {
		return "", err
	}
	if len(fileBytes) == 0 {
		return "", ErrDockerfileEmpty
	}
	var (
		dockerfile = lineContinuation.ReplaceAllString(stripComments(fileBytes), "")
		stepN      = 0
	)
	for _, line := range strings.Split(dockerfile, "\n") {
		line = strings.Trim(strings.Replace(line, "\t", " ", -1), " \t\r\n")
		if len(line) == 0 {
			continue
		}
		if err := b.BuildStep(fmt.Sprintf("%d", stepN), line); err != nil {
			if b.forceRm {
				b.clearTmp(b.tmpContainers)
			}
			return "", err
		} else if b.rm {
			b.clearTmp(b.tmpContainers)
		}
		stepN += 1
	}
	if b.image != "" {
		fmt.Fprintf(b.outStream, "Successfully built %s\n", utils.TruncateID(b.image))
		return b.image, nil
	}
	return "", fmt.Errorf("No image was generated. This may be because the Dockerfile does not, like, do anything.\n")
}

// BuildStep parses a single build step from `instruction` and executes it in the current context.
func (b *buildFile) BuildStep(name, expression string) error {
	fmt.Fprintf(b.outStream, "Step %s : %s\n", name, expression)
	tmp := strings.SplitN(expression, " ", 2)
	if len(tmp) != 2 {
		return fmt.Errorf("Invalid Dockerfile format")
	}
	instruction := strings.ToLower(strings.Trim(tmp[0], " "))
	arguments := strings.Trim(tmp[1], " ")

	method, exists := reflect.TypeOf(b).MethodByName("Cmd" + strings.ToUpper(instruction[:1]) + strings.ToLower(instruction[1:]))
	if !exists {
		fmt.Fprintf(b.errStream, "# Skipping unknown instruction %s\n", strings.ToUpper(instruction))
		return nil
	}

	ret := method.Func.Call([]reflect.Value{reflect.ValueOf(b), reflect.ValueOf(arguments)})[0].Interface()
	if ret != nil {
		return ret.(error)
	}

	fmt.Fprintf(b.outStream, " ---> %s\n", utils.TruncateID(b.image))
	return nil
}

func stripComments(raw []byte) string {
	var (
		out   []string
		lines = strings.Split(string(raw), "\n")
	)
	for _, l := range lines {
		if len(l) == 0 || l[0] == '#' {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

func copyAsDirectory(source, destination string, destinationExists bool) error {
	if err := archive.CopyWithTar(source, destination); err != nil {
		return err
	}

	if destinationExists {
		files, err := ioutil.ReadDir(source)
		if err != nil {
			return err
		}

		for _, file := range files {
			if err := fixPermissions(filepath.Join(destination, file.Name()), 0, 0); err != nil {
				return err
			}
		}
		return nil
	}

	return fixPermissions(destination, 0, 0)
}

func fixPermissions(destination string, uid, gid int) error {
	return filepath.Walk(destination, func(path string, info os.FileInfo, err error) error {
		if err := os.Lchown(path, uid, gid); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	})
}

func NewBuildFile(d *Daemon, eng *engine.Engine, outStream, errStream io.Writer, verbose, utilizeCache, rm bool, forceRm bool, outOld io.Writer, sf *utils.StreamFormatter, auth *registry.AuthConfig, authConfigFile *registry.ConfigFile) BuildFile {
	return &buildFile{
		daemon:        d,
		eng:           eng,
		config:        &runconfig.Config{},
		outStream:     outStream,
		errStream:     errStream,
		tmpContainers: make(map[string]struct{}),
		tmpImages:     make(map[string]struct{}),
		verbose:       verbose,
		utilizeCache:  utilizeCache,
		rm:            rm,
		forceRm:       forceRm,
		sf:            sf,
		authConfig:    auth,
		configFile:    authConfigFile,
		outOld:        outOld,
	}
}
