package loader

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/lyraproj/puppet-evaluator/types"
	"github.com/lyraproj/puppet-workflow/puppet"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/lyraproj/issue/issue"
	"github.com/lyraproj/puppet-evaluator/eval"
	"github.com/lyraproj/puppet-evaluator/yaml"
	"github.com/lyraproj/servicesdk/grpc"
	"github.com/lyraproj/servicesdk/serviceapi"
)

var embeddedPluginNames = []string{
	"identity",
	"puppet",
}

var defaultLoadPath = []string{"./plugins", "./build"}

// Loader implements the Loader API from go-servicesdk
type Loader struct {
	eval.DefiningLoader
	serviceCmds    map[string]string
	serviceCmdArgs map[string][]string
	pluginPath     []string
	logger         hclog.Logger
}

// New creates a loader instance
func New(parentLogger hclog.Logger, parentLoader eval.Loader) *Loader {
	logger := parentLogger.Named("loader")
	loader := &Loader{
		DefiningLoader: eval.NewParentedLoader(parentLoader),
		serviceCmds:    map[string]string{},
		serviceCmdArgs: map[string][]string{},
		pluginPath:     defaultLoadPath,
		logger:         logger,
	}
	return loader
}

// NameAuthority returns the name authority
func (l *Loader) NameAuthority() eval.URI {
	return l.DefiningLoader.NameAuthority()
}

// Parent returns the parent loader
func (l *Loader) Parent() eval.Loader {
	return l.DefiningLoader.(eval.ParentedLoader).Parent()
}

// LoadEntry returns the requested entry or nil if no such entry can be found
func (l *Loader) LoadEntry(c eval.Context, name eval.TypedName) eval.LoaderEntry {
	entry := l.DefiningLoader.LoadEntry(c, name)
	if entry != nil && entry.Value() != nil {
		return entry
	}

	if name.Namespace() != eval.NsService {
		return nil
	}

	s := l.loadService(c, name)
	if s == nil {
		return entry
	}
	return eval.NewLoaderEntry(s, nil)
}

// LoadService will load the named service. The caller is responsible for unloading it.
func (l *Loader) loadService(c eval.Context, serviceID eval.TypedName) serviceapi.Service {
	cmd, foundCmd := l.serviceCmds[serviceID.MapKey()]
	cmdArgs, _ := l.serviceCmdArgs[serviceID.MapKey()]
	if !foundCmd {
		l.logger.Error("unknown service id", "serviceID", serviceID)
		return nil
	}
	var serviceCmd *exec.Cmd
	if cmdArgs == nil {
		serviceCmd = exec.CommandContext(c, cmd)
	} else {
		serviceCmd = exec.CommandContext(c, cmd, cmdArgs...)
	}
	// FIXME Load should probably handle the context
	service, err := grpc.Load(serviceCmd, nil)
	if err != nil {
		l.logger.Error("service could not be started", "serviceID", serviceID, "err", err)
		return nil
	}
	return service
}

// PreLoad loads all plugins and manifests within reach.
func (l *Loader) PreLoad(c eval.Context) {
	// Use this loader when loading all typesets and definitions
	c.DoWithLoader(l, func() {
		// Embedded plugins
		l.loadEmbeddedPlugins(c)

		// Go plugins
		l.loadPlugins(c)

		// Puppet DSL files
		l.loadPuppetDSL(c)

		// Lyra Links
		l.loadLyraLinks(c)

		// Loading services based on other transports or dedicated loaders happens here
		// e.g. REST, serverless, Typescript ...
	})
}

// PreLoadPlugins loads all plugins within reach.
func (l *Loader) PreLoadPlugins(c eval.Context) {
	// Use this loader when loading all typesets and definitions
	c.DoWithLoader(l, func() {
		// Embedded plugins
		l.loadEmbeddedPlugins(c)

		// Go plugins
		l.loadPlugins(c)
	})
}

func (l *Loader) loadEmbeddedPlugins(c eval.Context) {
	l.logger.Debug("reading embedded plugins")
	l.logger.Debug(fmt.Sprintf("found %d embedded plugins", len(embeddedPluginNames)))
	for _, plugin := range embeddedPluginNames {
		cmd := os.Args[0] // This is this binary itself
		err := l.loadLiveMetadataFromPlugin(c, cmd, "--debug", "plugin", plugin)
		if err != nil {
			l.logger.Error("failed to load embedded plugin", "cmd", cmd, "plugin", plugin)
		}
	}
}

func (l *Loader) loadPlugins(c eval.Context) {
	l.logger.Debug("reading plugins from filesystem")
	plugins := l.findFiles("goplugin-*")
	for _, plugin := range plugins {
		err := l.loadMetadataFromPlugin(c, plugin)
		if err != nil {
			l.logger.Error("failed to load plugin", "plugin", plugin)
		}
	}
}

type subService struct {
	def serviceapi.Definition
}

func (s *subService) Parent(c eval.Context) serviceapi.Service {
	x, ok := eval.Load(c, s.def.ServiceId())
	if !ok {
		panic(fmt.Errorf("failed to load %s", s.def.ServiceId()))
	}
	return x.(serviceapi.Service)
}

func (s *subService) Invoke(c eval.Context, identifier, name string, arguments ...eval.Value) eval.Value {
	args := make([]eval.Value, 2, 2+len(arguments))
	args[0] = types.WrapString(identifier)
	args[1] = types.WrapString(name)
	args = append(args, arguments...)
	return s.Parent(c).Invoke(c, s.def.Identifier().Name(), "invoke", args...)
}

func (s *subService) Metadata(c eval.Context) (typeSet eval.TypeSet, definitions []serviceapi.Definition) {
	v := s.Parent(c).Invoke(c, s.def.Identifier().Name(), "metadata").(eval.List)
	if ts, ok := v.At(0).(eval.TypeSet); ok {
		typeSet = ts
	}
	if dl, ok := v.At(1).(eval.List); ok {
		definitions = make([]serviceapi.Definition, dl.Len())
		dl.EachWithIndex(func(d eval.Value, i int) {
			definitions[i] = d.(serviceapi.Definition)
		})
	}
	return
}

func (s *subService) State(c eval.Context, name string, input eval.OrderedMap) eval.PuppetObject {
	return s.Parent(c).Invoke(c, s.def.Identifier().Name(), "state", types.WrapString(name), input).(eval.PuppetObject)
}

func (s *subService) Identifier(eval.Context) eval.TypedName {
	return eval.NewTypedName(eval.NsService, s.def.Identifier().Name())
}

func (l *Loader) loadLyraLinks(c eval.Context) {
	llFiles := l.findFiles("*.ll")
	for _, lf := range llFiles {
		l.logger.Debug("reading Lyra Link", "file", lf)
		bts := types.BinaryFromFile(c, lf)
		link, ok := yaml.Unmarshal(c, bts.Bytes()).(eval.OrderedMap)
		if !ok {
			l.logger.Error("Lyra Link did not contain a map", "file", lf)
			continue
		}
		exe := ``
		if v, ok := link.Get4(`executable`); ok {
			if s, ok := v.(eval.StringValue); ok {
				exe = s.String()
			}
		}
		if exe == `` {
			l.logger.Error("Lyra Link did not contain a valid 'executable' entry", "file", lf)
			continue
		}
		exe = os.ExpandEnv(exe)
		args := []string{}
		if v, ok := link.Get4(`arguments`); ok {
			// Accepts array of strings or a string
			if a, ok := v.(*types.ArrayValue); ok {
				args = make([]string, a.Len())
				a.EachWithIndex(func(s eval.Value, i int) { args[i] = os.ExpandEnv(s.String()) })
			} else if s, ok := v.(eval.StringValue); ok {
				args = []string{os.ExpandEnv(s.String())}
			}
		}
		err := l.loadLiveMetadataFromPlugin(c, exe, args...)
		if err != nil {
			l.logger.Error("failed to load Lyra Link", "file", lf, "err", err)
		}
	}
}

func (l *Loader) loadPuppetDSL(c eval.Context) {
	x, ok := eval.Load(c, eval.NewTypedName(eval.NsService, `Puppet`))
	if !ok {
		l.logger.Error("failed to load Puppet DSL Service plugin")
		return
	}
	ppServer := x.(serviceapi.Service)

	l.logger.Debug("reading Puppet DSL and YAML from filesystem")

	ppFiles := l.findFiles("*.pp")

	yamlFiles := l.findFiles("*.yaml")

	allFiles := append(ppFiles, yamlFiles...)
	if len(allFiles) == 0 {
		return
	}

	for _, f := range allFiles {
		l.logger.Debug("loading manifest", "file", f)
		def := ppServer.Invoke(
			c, puppet.ManifestLoaderID, `loadManifest`,
			types.WrapString(filepath.Dir(f)),
			types.WrapString(f)).(serviceapi.Definition)
		sa := &subService{def}
		l.SetEntry(sa.Identifier(c), eval.NewLoaderEntry(sa, nil))
		l.loadMetadata(c, ``, nil, sa)
	}
}

func (l *Loader) findFiles(glob string) []string {
	files := []string{}
	for _, pluginDir := range l.pluginPath {
		// Check for a nested 'types' dir first
		typesDir := filepath.Join(pluginDir, "types")
		stat, err := os.Stat(typesDir)
		if err == nil && stat.IsDir() {
			l.logger.Debug(fmt.Sprintf("checking '%s' for '%s' files ...", typesDir, glob))
			fullGlob := filepath.Join(typesDir, glob)
			fs, err := filepath.Glob(fullGlob)
			if err != nil {
				l.logger.Error("failed to load plugins from types dir", "typesDir", typesDir, "err", err)
				continue
			}
			files = append(files, fs...)
			l.logger.Debug(fmt.Sprintf("found %d files", len(fs)))
		}

		// Now load from the specified plugin dir
		l.logger.Debug(fmt.Sprintf("checking '%s' for '%s' files ...", pluginDir, glob))
		stat, err = os.Stat(pluginDir)
		if err != nil && os.IsNotExist(err) {
			l.logger.Error("specified plugins directory not found, ignoring", "pluginDir", pluginDir)
			continue
		}
		if !stat.IsDir() {
			l.logger.Error("specified plugins directory is not actually a directory, ignoring", "pluginDir", pluginDir)
			continue
		}
		fullGlob := filepath.Join(pluginDir, glob)
		fs, err := filepath.Glob(fullGlob)
		if err != nil {
			l.logger.Error("failed to load plugins from dir", "pluginDir", pluginDir, "err", err)
			continue
		}

		files = append(files, fs...)
		l.logger.Debug(fmt.Sprintf("found %d files", len(fs)))
	}
	return files
}

func (l *Loader) loadMetadataFromPlugin(c eval.Context, cmd string, cmdArgs ...string) error {
	context, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()

	// FIXME Load should probably handle the eval.Context
	serviceCmd := exec.CommandContext(context, cmd, cmdArgs...)
	service, err := grpc.Load(serviceCmd, nil)
	if err != nil {
		return err
	}
	l.logger.Debug("loading metadata", "plugin", cmd)
	l.loadMetadata(c, cmd, cmdArgs, service)
	l.logger.Debug("done loading metadata", "plugin", cmd)
	return nil
}

func (l *Loader) loadLiveMetadataFromPlugin(c eval.Context, cmd string, cmdArgs ...string) error {
	// FIXME Load should probably handle the eval.Context
	serviceCmd := exec.CommandContext(c, cmd, cmdArgs...)
	service, err := grpc.Load(serviceCmd, nil)
	if err != nil {
		return err
	}
	l.SetEntry(service.Identifier(c), eval.NewLoaderEntry(service, nil))

	l.logger.Debug("loading metadata", "plugin", cmd)
	l.loadMetadata(c, cmd, cmdArgs, service)
	l.logger.Debug("done loading metadata", "plugin", cmd)
	return nil
}

func (l *Loader) loadMetadata(c eval.Context, cmd string, cmdArgs []string, service serviceapi.Service) {
	_, defs := service.Metadata(c)
	if len(defs) == 0 {
		return
	}
	serviceID := defs[0].ServiceId().MapKey()

	// Register service
	if cmd != `` {
		if _, ok := l.serviceCmds[serviceID]; ok {
			l.logger.Error("a service has already been registered with this service id", "serviceID", serviceID)
			return
		}
		l.serviceCmds[serviceID] = cmd
		l.serviceCmdArgs[serviceID] = cmdArgs
		l.logger.Debug("registered service", "serviceID", serviceID, "count", len(l.serviceCmds))
	}

	// Register definitions
	for _, def := range defs {
		l.SetEntry(def.Identifier(), eval.NewLoaderEntry(def, nil))
		l.logger.Debug("registered definition", "definition", def.Identifier())

		if handlerFor, ok := def.Properties().Get4(`handlerFor`); ok {
			hn := eval.NewTypedName(eval.NsHandler, handlerFor.(issue.Named).Name())
			l.SetEntry(hn, eval.NewLoaderEntry(def, nil))
			l.logger.Debug("registered handler", "definition", def.Identifier(), "handler for", hn)
		}
	}
}
