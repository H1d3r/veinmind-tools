package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/chaitin/libveinmind/go"
	"github.com/chaitin/libveinmind/go/cmd"
	"github.com/chaitin/libveinmind/go/containerd"
	"github.com/chaitin/libveinmind/go/docker"
	"github.com/chaitin/libveinmind/go/plugin"
	"github.com/chaitin/libveinmind/go/plugin/log"
	"github.com/chaitin/libveinmind/go/plugin/service"
	"github.com/chaitin/veinmind-tools/veinmind-common/go/service/report"
	"github.com/chaitin/veinmind-tools/veinmind-runner/pkg/registry"
	"github.com/chaitin/veinmind-tools/veinmind-runner/pkg/reporter"
	"github.com/distribution/distribution/reference"
	"github.com/spf13/cobra"
	"os"
	"path"
	"strings"
)

var (
	ps             []*plugin.Plugin
	ctx            context.Context
	runnerReporter *reporter.Reporter
	reportService  *report.ReportService
	scanPreRunE    = func(c *cobra.Command, args []string) error {
		// Discover Plugins
		ctx = c.Context()
		glob, err := c.Flags().GetString("glob")
		if err == nil && glob != "" {
			ps, err = plugin.DiscoverPlugins(ctx, ".", plugin.WithGlob(glob))
		} else {
			ps, err = plugin.DiscoverPlugins(ctx, ".")
		}
		if err != nil {
			return err
		}
		for _, p := range ps {
			log.Infof("Discovered plugin: %#v\n", p.Name)
		}

		// Reporter Channel Listen
		go runnerReporter.Listen()

		// Event Channel Listen
		go func() {
			for {
				select {
				case evt := <-reportService.EventChannel:
					runnerReporter.EventChannel <- evt
				}
			}
		}()

		return nil
	}
	scanPostRunE = func(cmd *cobra.Command, args []string) error {
		// Stop reporter listen
		runnerReporter.StopListen()

		// Output
		err := runnerReporter.Write(os.Stdout)
		if err != nil {
			log.Error(err)
		}
		output, _ := cmd.Flags().GetString("output")
		if _, err := os.Stat(output); errors.Is(err, os.ErrNotExist) {
			f, err := os.Create(output)
			if err != nil {
				log.Error(err)
			} else {
				err = runnerReporter.Write(f)
				if err != nil {
					return err
				}
			}
		} else {
			f, err := os.OpenFile(output, os.O_WRONLY, 0666)
			if err != nil {
				log.Error(err)
			} else {
				err = runnerReporter.Write(f)
				if err != nil {
					return err
				}
			}
		}

		// Exit
		exitcode, err := cmd.Flags().GetInt("exit-code")
		if err != nil {
			return err
		}

		if exitcode == 0 {
			return nil
		} else {
			events, err := runnerReporter.GetEvents()
			if err != nil {
				return err
			}

			if len(events) > 0 {
				os.Exit(exitcode)
			} else {
				return nil
			}
		}

		return nil
	}
)

var rootCmd = &cmd.Command{}
var listCmd = &cmd.Command{
	Use:   "list",
	Short: "list relevant information",
}
var listPluginCmd = &cmd.Command{
	Use:   "plugin",
	Short: "list plugin information",
	RunE: func(cmd *cobra.Command, args []string) error {
		ps, err := plugin.DiscoverPlugins(context.Background(), ".")
		if err != nil {
			return err
		}

		verbose, err := cmd.Flags().GetBool("verbose")
		if err != nil {
			return err
		}

		for _, p := range ps {
			if verbose {
				pJsonByte, err := json.MarshalIndent(p, "", "	")
				if err != nil {
					log.Error(err)
					continue
				}
				log.Info("\n" + string(pJsonByte))
			} else {
				log.Info("Plugin Name: " + p.Name)
			}
		}

		return nil
	},
}
var scanHostCmd = &cmd.Command{
	Use:      "scan-host",
	Short:    "perform hosted scan command",
	PreRunE:  scanPreRunE,
	PostRunE: scanPostRunE,
}
var scanRegistryCmd = &cmd.Command{
	Use:     "scan-registry",
	Short:   "perform registry scan command",
	PreRunE: scanPreRunE,
	RunE: func(cmd *cobra.Command, args []string) error {
		var (
			err             error
			c               registry.Client
			veinmindRuntime api.Runtime
		)

		server, _ := cmd.Flags().GetString("server")
		config, _ := cmd.Flags().GetString("config")
		namespace, _ := cmd.Flags().GetString("namespace")
		runtime, _ := cmd.Flags().GetString("runtime")
		// tags, _ := cmd.Flags().GetStringSlice("tags")

		switch runtime {
		case "docker":
			if config == "" {
				c, err = registry.NewRegistryDockerClient()
			} else {
				c, err = registry.NewRegistryDockerClient(registry.WithAuth(config))
			}
			if err != nil {
				return err
			}

			veinmindRuntime, err = docker.New()
			if err != nil {
				return err
			}
		case "containerd":
			c, err = registry.NewRegistryContainerdClient()
			if err != nil {
				return err
			}

			veinmindRuntime, err = containerd.New()
			if err != nil {
				return err
			}
		default:
			return errors.New("runtime not match")
		}

		// If no repo is specified, then query all repo through catalog
		repos := []string{}
		if len(args) == 0 {
			switch c := c.(type) {
			case *registry.RegistryDockerClient:
				repos, err = c.GetRepos(server)
				if err != nil {
					return err
				}
			}
		} else {
			// If it doesn't start with registry, autofill registry
			for _, r := range args {
				rParse, err := reference.Parse(r)
				if err != nil {
					log.Error(err)
					continue
				}

				repos = append(repos, rParse.String())
			}
		}

		if namespace != "" {
			namespaceMaps := map[string][]string{}
			for _, repo := range repos {
				rNamed, err := reference.ParseNamed(repo)
				if err != nil {
					log.Error(err)
					continue
				}

				p := reference.Path(rNamed)
				ns := strings.Split(p, "/")[0]
				namespaceMaps[ns] = append(namespaceMaps[ns], repo)
			}

			_, ok := namespaceMaps[namespace]
			if ok {
				repos = namespaceMaps[namespace]
			} else {
				return errors.New("Namespace doesn't match any repos")
			}
		}

		for _, repo := range repos {
			log.Infof("Start pull image: %#v\n", repo)
			r, err := c.Pull(repo)
			if err != nil {
				log.Errorf("Pull image error: %#v\n", err.Error())
				continue
			}
			log.Infof("Pull image success: %#v\n", repo)

			var (
				rNamed reference.Named
			)

			switch c.(type) {
			case *registry.RegistryDockerClient:
				rNamed, err = reference.ParseDockerRef(r)
				if err != nil {
					log.Error(err)
					continue
				}

				domain := reference.Domain(rNamed)
				if domain == "index.docker.io" || domain == "docker.io" {
					repo = reference.Path(rNamed)
					if (strings.Split(repo, "/")[0] == "library" || strings.Split(repo, "/")[0] == "_") && len(strings.Split(repo, "/")) >= 2 {
						repo = strings.Join(strings.Split(repo, "/")[1:], "")
					}
				}
			case *registry.RegistryContainerdClient:
				repo = r
			}

			ids, err := veinmindRuntime.FindImageIDs(repo)
			switch c.(type) {
			case *registry.RegistryDockerClient:
				if len(ids) > 0 {
					for _, id := range ids {
						image, err := veinmindRuntime.OpenImageByID(id)
						if err != nil {
							log.Error(err)
							continue
						}

						err = scan(cmd, image)
						if err != nil {
							log.Error(err)
							continue
						}
					}

					for _, id := range ids {
						err = c.Remove(id)
						if err != nil {
							log.Error(err)
						} else {
							log.Infof("Remove image success: %#v\n", repo)
						}
					}
				}
			case *registry.RegistryContainerdClient:
				image, err := veinmindRuntime.OpenImageByID(r)
				if err != nil {
					log.Error(err)
					continue
				}

				var (
					repoRef string
				)
				repoRefs, err := image.RepoRefs()
				if len(repoRefs) > 0 {
					repoRef = repoRefs[0]
				} else {
					repoRef = image.ID()
				}

				err = scan(cmd, image)
				if err != nil {
					log.Error(err)
				}

				err = c.Remove(repoRef)
				if err != nil {
					log.Error(err)
				} else {
					log.Infof("Remove image success: %#v\n", repo)
				}
			}
		}

		return nil
	},
	PostRunE: scanPostRunE,
}

func scan(c *cmd.Command, image api.Image) error {
	refs, err := image.RepoRefs()
	ref := ""
	if err == nil && len(refs) > 0 {
		ref = refs[0]
	} else {
		ref = image.ID()
	}

	// Get threads value
	t, err := c.Flags().GetInt("threads")
	if err != nil {
		t = 5
	}

	log.Infof("Scan image: %#v\n", ref)
	if err := cmd.ScanImage(ctx, ps, image,
		plugin.WithExecInterceptor(func(
			ctx context.Context, plug *plugin.Plugin, c *plugin.Command,
			next func(context.Context, ...plugin.ExecOption) error,
		) error {
			// Register Service
			reg := service.NewRegistry()
			reg.AddServices(log.WithFields(log.Fields{
				"plugin":  plug.Name,
				"command": path.Join(c.Path...),
			}))
			reg.AddServices(reportService)

			// Next Plugin
			return next(ctx, reg.Bind())
		}), plugin.WithExecParallelism(t)); err != nil {
		return err
	}
	return nil
}

func init() {
	// Cobra init
	rootCmd.AddCommand(cmd.MapImageCommand(scanHostCmd, scan))
	rootCmd.AddCommand(scanRegistryCmd)
	rootCmd.AddCommand(listCmd)
	rootCmd.PersistentFlags().IntP("exit-code", "e", 0, "exit-code when veinmind-runner find security issues")
	listCmd.AddCommand(listPluginCmd)
	listPluginCmd.Flags().BoolP("verbose", "v", false, "verbose mode")
	scanHostCmd.Flags().StringP("glob", "g", "", "specifies the pattern of plugin file to find")
	scanHostCmd.Flags().StringP("output", "o", "report.json", "output filepath of report")
	scanHostCmd.Flags().IntP("threads", "t", 5, "threads for scan action")
	scanRegistryCmd.Flags().StringP("runtime", "r", "docker", "specifies the runtime of registry client to use")
	scanRegistryCmd.Flags().StringP("glob", "g", "", "specifies the pattern of plugin file to find")
	scanRegistryCmd.Flags().StringP("output", "o", "report.json", "output filepath of report")
	scanRegistryCmd.Flags().StringP("server", "s", "index.docker.io", "server address of registry")
	scanRegistryCmd.Flags().StringP("config", "c", "", "auth config path")
	scanRegistryCmd.Flags().StringP("namespace", "n", "", "namespace of repo")
	scanRegistryCmd.Flags().StringSliceP("tags", "t", []string{"latest"}, "tags of repo")
	scanRegistryCmd.Flags().Int("threads", 5, "threads for scan action")

	// Service client init
	reportService = report.NewReportService()

	// Reporter init
	r, err := reporter.NewReporter()
	if err != nil {
		log.Fatal(err)
	}
	runnerReporter = r
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
