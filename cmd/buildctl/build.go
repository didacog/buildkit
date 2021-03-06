package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"io"
	"os"
	"strings"

	"github.com/containerd/console"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth/authprovider"
	"github.com/moby/buildkit/session/secrets/secretsprovider"
	"github.com/moby/buildkit/session/sshforward/sshprovider"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/util/entitlements"
	"github.com/moby/buildkit/util/progress/progressui"
	"github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	"golang.org/x/sync/errgroup"
)

var buildCommand = cli.Command{
	Name:   "build",
	Usage:  "build",
	Action: build,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "exporter",
			Usage: "Define exporter for build result",
		},
		cli.StringSliceFlag{
			Name:  "exporter-opt",
			Usage: "Define custom options for exporter",
		},
		cli.StringFlag{
			Name:  "progress",
			Usage: "Set type of progress (auto, plain, tty). Use plain to show container output",
			Value: "auto",
		},
		cli.StringFlag{
			Name:  "trace",
			Usage: "Path to trace file. Defaults to no tracing.",
		},
		cli.StringSliceFlag{
			Name:  "local",
			Usage: "Allow build access to the local directory",
		},
		cli.StringFlag{
			Name:  "frontend",
			Usage: "Define frontend used for build",
		},
		cli.StringSliceFlag{
			Name:  "frontend-opt",
			Usage: "Define custom options for frontend",
		},
		cli.BoolFlag{
			Name:  "no-cache",
			Usage: "Disable cache for all the vertices",
		},
		cli.StringSliceFlag{
			Name:  "export-cache",
			Usage: "Export build cache, e.g. type=registry,ref=example.com/foo/bar, or type=local,store=path/to/dir",
		},
		cli.StringSliceFlag{
			Name:   "export-cache-opt",
			Usage:  "Define custom options for cache exporting (DEPRECATED: use --export-cache type=<type>,<opt>=<optval>[,<opt>=<optval>]",
			Hidden: true,
		},
		cli.StringSliceFlag{
			Name:  "import-cache",
			Usage: "Import build cache",
		},
		cli.StringSliceFlag{
			Name:  "secret",
			Usage: "Secret value exposed to the build. Format id=secretname,src=filepath",
		},
		cli.StringSliceFlag{
			Name:  "allow",
			Usage: "Allow extra privileged entitlement, e.g. network.host, security.unconfined",
		},
		cli.StringSliceFlag{
			Name:  "ssh",
			Usage: "Allow forwarding SSH agent to the builder. Format default|<id>[=<socket>|<key>[,<key>]]",
		},
	},
}

func read(r io.Reader, clicontext *cli.Context) (*llb.Definition, error) {
	def, err := llb.ReadFrom(r)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse input")
	}
	if clicontext.Bool("no-cache") {
		for _, dt := range def.Def {
			var op pb.Op
			if err := (&op).Unmarshal(dt); err != nil {
				return nil, errors.Wrap(err, "failed to parse llb proto op")
			}
			dgst := digest.FromBytes(dt)
			opMetadata, ok := def.Metadata[dgst]
			if !ok {
				opMetadata = pb.OpMetadata{}
			}
			c := llb.Constraints{Metadata: opMetadata}
			llb.IgnoreCache(&c)
			def.Metadata[dgst] = c.Metadata
		}
	}
	return def, nil
}

func openTraceFile(clicontext *cli.Context) (*os.File, error) {
	if traceFileName := clicontext.String("trace"); traceFileName != "" {
		return os.OpenFile(traceFileName, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	}
	return nil, nil
}

func build(clicontext *cli.Context) error {
	c, err := resolveClient(clicontext)
	if err != nil {
		return err
	}

	traceFile, err := openTraceFile(clicontext)
	if err != nil {
		return err
	}
	var traceEnc *json.Encoder
	if traceFile != nil {
		defer traceFile.Close()
		traceEnc = json.NewEncoder(traceFile)

		logrus.Infof("tracing logs to %s", traceFile.Name())
	}

	attachable := []session.Attachable{authprovider.NewDockerAuthProvider()}

	if ssh := clicontext.StringSlice("ssh"); len(ssh) > 0 {
		configs, err := parseSSHSpecs(ssh)
		if err != nil {
			return err
		}
		sp, err := sshprovider.NewSSHAgentProvider(configs)
		if err != nil {
			return err
		}
		attachable = append(attachable, sp)
	}

	if secrets := clicontext.StringSlice("secret"); len(secrets) > 0 {
		secretProvider, err := parseSecretSpecs(secrets)
		if err != nil {
			return err
		}
		attachable = append(attachable, secretProvider)
	}

	allowed, err := parseEntitlements(clicontext.StringSlice("allow"))
	if err != nil {
		return err
	}

	cacheExports, err := parseExportCache(clicontext.StringSlice("export-cache"), clicontext.StringSlice("export-cache-opt"))
	if err != nil {
		return err
	}
	cacheImports, err := parseImportCache(clicontext.StringSlice("import-cache"))
	if err != nil {
		return err
	}

	ch := make(chan *client.SolveStatus)
	eg, ctx := errgroup.WithContext(commandContext(clicontext))

	solveOpt := client.SolveOpt{
		Exporter: clicontext.String("exporter"),
		// ExporterAttrs is set later
		// LocalDirs is set later
		Frontend: clicontext.String("frontend"),
		// FrontendAttrs is set later
		CacheExports:        cacheExports,
		CacheImports:        cacheImports,
		Session:             attachable,
		AllowedEntitlements: allowed,
	}
	solveOpt.ExporterAttrs, err = attrMap(clicontext.StringSlice("exporter-opt"))
	if err != nil {
		return errors.Wrap(err, "invalid exporter-opt")
	}
	solveOpt.ExporterOutput, solveOpt.ExporterOutputDir, err = resolveExporterOutput(solveOpt.Exporter, solveOpt.ExporterAttrs["output"])
	if err != nil {
		return errors.Wrap(err, "invalid exporter-opt: output")
	}
	if solveOpt.ExporterOutput != nil || solveOpt.ExporterOutputDir != "" {
		delete(solveOpt.ExporterAttrs, "output")
	}

	solveOpt.FrontendAttrs, err = attrMap(clicontext.StringSlice("frontend-opt"))
	if err != nil {
		return errors.Wrap(err, "invalid frontend-opt")
	}

	solveOpt.LocalDirs, err = attrMap(clicontext.StringSlice("local"))
	if err != nil {
		return errors.Wrap(err, "invalid local")
	}

	var def *llb.Definition
	if clicontext.String("frontend") == "" {
		if fi, _ := os.Stdin.Stat(); (fi.Mode() & os.ModeCharDevice) != 0 {
			return errors.Errorf("please specify --frontend or pipe LLB definition to stdin")
		}
		def, err = read(os.Stdin, clicontext)
		if err != nil {
			return err
		}
		if len(def.Def) == 0 {
			return errors.Errorf("empty definition sent to build. Specify --frontend instead?")
		}
	} else {
		if clicontext.Bool("no-cache") {
			solveOpt.FrontendAttrs["no-cache"] = ""
		}
	}

	eg.Go(func() error {
		resp, err := c.Solve(ctx, def, solveOpt, ch)
		if err != nil {
			return err
		}
		for k, v := range resp.ExporterResponse {
			logrus.Debugf("exporter response: %s=%s", k, v)
		}
		return err
	})

	displayCh := ch
	if traceEnc != nil {
		displayCh = make(chan *client.SolveStatus)
		eg.Go(func() error {
			defer close(displayCh)
			for s := range ch {
				if err := traceEnc.Encode(s); err != nil {
					logrus.Error(err)
				}
				displayCh <- s
			}
			return nil
		})
	}

	eg.Go(func() error {
		var c console.Console
		progressOpt := clicontext.String("progress")

		switch progressOpt {
		case "auto", "tty":
			cf, err := console.ConsoleFromFile(os.Stderr)
			if err != nil && progressOpt == "tty" {
				return err
			}
			c = cf
		case "plain":
		default:
			return errors.Errorf("invalid progress value : %s", progressOpt)
		}

		// not using shared context to not disrupt display but let is finish reporting errors
		return progressui.DisplaySolveStatus(context.TODO(), "", c, os.Stdout, displayCh)
	})

	return eg.Wait()
}

func parseExportCacheCSV(s string) (client.CacheOptionsEntry, error) {
	ex := client.CacheOptionsEntry{
		Type:  "",
		Attrs: map[string]string{},
	}
	csvReader := csv.NewReader(strings.NewReader(s))
	fields, err := csvReader.Read()
	if err != nil {
		return ex, err
	}
	for _, field := range fields {
		parts := strings.SplitN(field, "=", 2)
		key := strings.ToLower(parts[0])
		value := parts[1]
		switch key {
		case "type":
			ex.Type = value
		default:
			ex.Attrs[key] = value
		}
	}
	if ex.Type == "" {
		return ex, errors.New("--export-cache requires type=<type>")
	}
	if _, ok := ex.Attrs["mode"]; !ok {
		ex.Attrs["mode"] = "min"
	}
	return ex, nil
}

func parseExportCache(exportCaches, legacyExportCacheOpts []string) ([]client.CacheOptionsEntry, error) {
	var exports []client.CacheOptionsEntry
	if len(legacyExportCacheOpts) > 0 {
		if len(exportCaches) != 1 {
			return nil, errors.New("--export-cache-opt requires exactly single --export-cache")
		}
	}
	for _, exportCache := range exportCaches {
		legacy := !strings.Contains(exportCache, "type=")
		if legacy {
			logrus.Warnf("--export-cache <ref> --export-cache-opt <opt>=<optval> is deprecated. Please use --export-cache type=registry,ref=<ref>,<opt>=<optval>[,<opt>=<optval>] instead.")
			attrs, err := attrMap(legacyExportCacheOpts)
			if err != nil {
				return nil, err
			}
			if _, ok := attrs["mode"]; !ok {
				attrs["mode"] = "min"
			}
			attrs["ref"] = exportCache
			exports = append(exports, client.CacheOptionsEntry{
				Type:  "registry",
				Attrs: attrs,
			})
		} else {
			if len(legacyExportCacheOpts) > 0 {
				return nil, errors.New("--export-cache-opt is not supported for the specified --export-cache. Please use --export-cache type=<type>,<opt>=<optval>[,<opt>=<optval>] instead.")
			}
			ex, err := parseExportCacheCSV(exportCache)
			if err != nil {
				return nil, err
			}
			exports = append(exports, ex)
		}
	}
	return exports, nil

}

func parseImportCacheCSV(s string) (client.CacheOptionsEntry, error) {
	im := client.CacheOptionsEntry{
		Type:  "",
		Attrs: map[string]string{},
	}
	csvReader := csv.NewReader(strings.NewReader(s))
	fields, err := csvReader.Read()
	if err != nil {
		return im, err
	}
	for _, field := range fields {
		parts := strings.SplitN(field, "=", 2)
		key := strings.ToLower(parts[0])
		value := parts[1]
		switch key {
		case "type":
			im.Type = value
		default:
			im.Attrs[key] = value
		}
	}
	if im.Type == "" {
		return im, errors.New("--import-cache requires type=<type>")
	}
	return im, nil
}

func parseImportCache(importCaches []string) ([]client.CacheOptionsEntry, error) {
	var imports []client.CacheOptionsEntry
	for _, importCache := range importCaches {
		legacy := !strings.Contains(importCache, "type=")
		if legacy {
			logrus.Warnf("--import-cache <ref> is deprecated. Please use --import-cache type=registry,ref=<ref>,<opt>=<optval>[,<opt>=<optval>] instead.")
			imports = append(imports, client.CacheOptionsEntry{
				Type:  "registry",
				Attrs: map[string]string{"ref": importCache},
			})
		} else {
			im, err := parseImportCacheCSV(importCache)
			if err != nil {
				return nil, err
			}
			imports = append(imports, im)
		}
	}
	return imports, nil
}

func attrMap(sl []string) (map[string]string, error) {
	m := map[string]string{}
	for _, v := range sl {
		parts := strings.SplitN(v, "=", 2)
		if len(parts) != 2 {
			return nil, errors.Errorf("invalid value %s", v)
		}
		m[parts[0]] = parts[1]
	}
	return m, nil
}

func parseSecretSpecs(sl []string) (session.Attachable, error) {
	fs := make([]secretsprovider.FileSource, 0, len(sl))
	for _, v := range sl {
		s, err := parseSecret(v)
		if err != nil {
			return nil, err
		}
		fs = append(fs, *s)
	}
	store, err := secretsprovider.NewFileStore(fs)
	if err != nil {
		return nil, err
	}
	return secretsprovider.NewSecretProvider(store), nil
}

func parseSecret(value string) (*secretsprovider.FileSource, error) {
	csvReader := csv.NewReader(strings.NewReader(value))
	fields, err := csvReader.Read()
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse csv secret")
	}

	fs := secretsprovider.FileSource{}

	for _, field := range fields {
		parts := strings.SplitN(field, "=", 2)
		key := strings.ToLower(parts[0])

		if len(parts) != 2 {
			return nil, errors.Errorf("invalid field '%s' must be a key=value pair", field)
		}

		value := parts[1]
		switch key {
		case "type":
			if value != "file" {
				return nil, errors.Errorf("unsupported secret type %q", value)
			}
		case "id":
			fs.ID = value
		case "source", "src":
			fs.FilePath = value
		default:
			return nil, errors.Errorf("unexpected key '%s' in '%s'", key, field)
		}
	}
	return &fs, nil
}

// resolveExporterOutput returns at most either one of io.WriteCloser (single file) or a string (directory path).
func resolveExporterOutput(exporter, output string) (io.WriteCloser, string, error) {
	switch exporter {
	case client.ExporterLocal:
		if output == "" {
			return nil, "", errors.New("output directory is required for local exporter")
		}
		return nil, output, nil
	case client.ExporterOCI, client.ExporterDocker:
		if output != "" {
			fi, err := os.Stat(output)
			if err != nil && !os.IsNotExist(err) {
				return nil, "", errors.Wrapf(err, "invalid destination file: %s", output)
			}
			if err == nil && fi.IsDir() {
				return nil, "", errors.Errorf("destination file is a directory")
			}
			w, err := os.Create(output)
			return w, "", err
		}
		// if no output file is specified, use stdout
		if _, err := console.ConsoleFromFile(os.Stdout); err == nil {
			return nil, "", errors.Errorf("output file is required for %s exporter. refusing to write to console", exporter)
		}
		return os.Stdout, "", nil
	default: // e.g. client.ExporterImage
		if output != "" {
			return nil, "", errors.Errorf("output %s is not supported by %s exporter", output, exporter)
		}
		return nil, "", nil
	}
}

func parseEntitlements(inp []string) ([]entitlements.Entitlement, error) {
	ent := make([]entitlements.Entitlement, 0, len(inp))
	for _, v := range inp {
		e, err := entitlements.Parse(v)
		if err != nil {
			return nil, err
		}
		ent = append(ent, e)
	}
	return ent, nil
}

func parseSSHSpecs(inp []string) ([]sshprovider.AgentConfig, error) {
	configs := make([]sshprovider.AgentConfig, 0, len(inp))
	for _, v := range inp {
		parts := strings.SplitN(v, "=", 2)
		cfg := sshprovider.AgentConfig{
			ID: parts[0],
		}
		if len(parts) > 1 {
			cfg.Paths = strings.Split(parts[1], ",")
		}
		configs = append(configs, cfg)
	}
	return configs, nil
}
