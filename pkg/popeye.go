package pkg

import (
	"bufio"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/derailed/popeye/internal"
	"github.com/derailed/popeye/internal/issues"
	"github.com/derailed/popeye/internal/k8s"
	"github.com/derailed/popeye/internal/report"
	"github.com/derailed/popeye/internal/sanitize"
	"github.com/derailed/popeye/internal/scrub"
	"github.com/derailed/popeye/pkg/config"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

var (
	// LogFile the path to our logs.
	LogFile = filepath.Join(os.TempDir(), fmt.Sprintf("popeye.log"))
	// DumpDir indicates a directory location for sanitixer reports.
	DumpDir = dumpDir()
)

func dumpDir() string {
	if d := os.Getenv("POPEYE_REPORT_DIR"); d != "" {
		return d
	}
	return filepath.Join(os.TempDir(), "popeye")
}

type (
	scrubFn func(context.Context, *scrub.Cache, *issues.Codes) scrub.Sanitizer

	// Popeye a kubernetes sanitizer.
	Popeye struct {
		client       *k8s.Client
		config       *config.Config
		outputTarget *os.File
		log          *zerolog.Logger
		flags        *config.Flags
		builder      *report.Builder
		aliases      *internal.Aliases
	}
)

// NewPopeye returns a new sanitizer.
func NewPopeye(flags *config.Flags, log *zerolog.Logger) (*Popeye, error) {
	cfg, err := config.NewConfig(flags)
	if err != nil {
		return nil, err
	}

	a := internal.NewAliases()
	p := Popeye{
		client:  k8s.NewClient(flags),
		config:  cfg,
		log:     log,
		flags:   flags,
		aliases: a,
		builder: report.NewBuilder(a),
	}

	return &p, nil
}

// Init configures popeye prior to sanitization.
func (p *Popeye) Init() error {
	if !isSet(p.flags.Save) {
		return p.ensureOutput()
	}

	if err := ensurePath(DumpDir, 0755); err != nil {
		return err
	}
	return p.ensureOutput()
}

// Sanitize scans a cluster for potential issues.
func (p *Popeye) Sanitize() error {
	defer func() {
		if p.outputTarget != os.Stdout {
			if err := p.outputTarget.Close(); err != nil {
				log.Fatal().Err(err).Msg("Closing report")
			}
		}
	}()

	if err := p.sanitize(); err != nil {
		return err
	}

	return p.dump(true)
}

func (p *Popeye) dumpJunit() error {
	res, err := p.builder.ToJunit()
	if err != nil {
		return err
	}
	if _, err := p.outputTarget.WriteString(xml.Header); err != nil {
		return err
	}
	fmt.Fprintf(p.outputTarget, "%v\n", res)

	return nil
}

func (p *Popeye) dumpYAML() error {
	res, err := p.builder.ToYAML()
	if err != nil {
		return err
	}
	fmt.Fprintf(p.outputTarget, "%v\n", res)

	return nil
}

func (p *Popeye) dumpJSON() error {
	res, err := p.builder.ToJSON()
	if err != nil {
		return err
	}
	fmt.Fprintf(p.outputTarget, "%v\n", res)

	return nil
}

func (p *Popeye) dumpStd(mode, header bool) error {
	var (
		w = bufio.NewWriter(p.outputTarget)
		s = report.NewSanitizer(w, mode)
	)

	if header {
		p.builder.PrintHeader(s)
	}
	mx, err := p.client.ClusterHasMetrics()
	if err != nil {
		mx = false
	}
	p.builder.PrintClusterInfo(s, p.client.ActiveCluster(), mx)
	p.builder.PrintReport(config.Level(p.config.LinterLevel()), s)
	p.builder.PrintSummary(s)

	return w.Flush()
}

//address *string, level string, format, cluster, namespace string
// address *string, level *string, format, cluster, namespace string
func (p *Popeye) dumpPrometheus() error {
	pusher := p.builder.ToPrometheus(
		p.flags.PushGatewayAddress,
		p.flags.OutputDetail,
		p.flags.LintLevel,
		p.client.ActiveCluster(),
		p.client.ActiveNamespace(),
	)
	return pusher.Add()
}

// Dump prints out sanitizer report.
func (p *Popeye) dump(printHeader bool) error {
	if !p.builder.HasContent() {
		return errors.New("Nothing to report, check section name or permissions")
	}

	var err error
	switch p.flags.OutputFormat() {
	case report.JunitFormat:
		err = p.dumpJunit()
	case report.YAMLFormat:
		err = p.dumpYAML()
	case report.JSONFormat:
		err = p.dumpJSON()
	case report.PrometheusFormat:
		err = p.dumpPrometheus()
	default:
		err = p.dumpStd(p.flags.OutputFormat() == report.JurassicFormat, printHeader)
	}

	return err
}

func (p *Popeye) sanitizers() map[string]scrubFn {
	return map[string]scrubFn{
		"cluster":                 scrub.NewCluster,
		"configmap":               scrub.NewConfigMap,
		"secret":                  scrub.NewSecret,
		"deployment":              scrub.NewDeployment,
		"daemonset":               scrub.NewDaemonSet,
		"horizontalpodautoscaler": scrub.NewHorizontalPodAutoscaler,
		"namespace":               scrub.NewNamespace,
		"node":                    scrub.NewNode,
		"persistentvolume":        scrub.NewPersistentVolume,
		"persistentvolumeclaim":   scrub.NewPersistentVolumeClaim,
		"pod":                     scrub.NewPod,
		"replicaset":              scrub.NewReplicaSet,
		"service":                 scrub.NewService,
		"serviceaccount":          scrub.NewServiceAccount,
		"statefulset":             scrub.NewStatefulSet,
		"poddisruptionbudget":     scrub.NewPodDisruptionBudget,
		"ingress":                 scrub.NewIngress,
		"networkpolicy":           scrub.NewNetworkPolicy,
		"podsecuritypolicy":       scrub.NewPodSecurityPolicy,
		"clusterrole":             scrub.NewClusterRole,
		"clusterrolebinding":      scrub.NewClusterRoleBinding,
		"role":                    scrub.NewRole,
		"rolebinding":             scrub.NewRoleBinding,
	}
}

func (p *Popeye) ensureOutput() error {
	p.outputTarget = os.Stdout
	if !isSet(p.flags.Save) {
		return nil
	}

	if p.flags.Output == nil {
		*p.flags.Output = "standard"
	}

	ext := "txt"
	switch *p.flags.Output {
	case "json":
		ext = "json"
	case "junit":
		ext = "xml"
	case "yaml":
		ext = "yml"
	}

	const outFmt = "sanitizer_%s_%d.%s"
	f := filepath.Join(DumpDir, fmt.Sprintf(outFmt, p.client.ActiveCluster(), time.Now().UnixNano(), ext))
	var err error
	if p.outputTarget, err = os.Create(f); err != nil {
		return err
	}

	fmt.Printf("Sanitizer saved to: %s\n", f)
	return nil
}

func (p *Popeye) sanitize() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = context.WithValue(
		ctx,
		sanitize.PopeyeKey("OverAllocs"),
		*p.flags.CheckOverAllocs,
	)

	cache := scrub.NewCache(p.client, p.config)
	codes, err := issues.LoadCodes()
	if err != nil {
		return err
	}
	codes.Refine(p.config.Codes)
	sections := make([]string, 0, len(p.sanitizers()))
	for section := range p.sanitizers() {
		sections = append(sections, section)
	}
	sort.StringSlice(sections).Sort()
	for _, section := range sections {
		if !in(p.aliases.ToResources(p.config.Sections()), section) {
			continue
		}
		// Skip node checks if active namespace is set.
		if section == "node" && p.client.ActiveNamespace() != "" {
			continue
		}

		ctx = context.WithValue(ctx, internal.KeyRun, internal.RunInfo{Section: section})
		s := p.sanitizers()[section](ctx, cache, codes)
		if err := s.Sanitize(ctx); err != nil {
			p.builder.AddError(err)
			continue
		}

		tally := report.NewTally()
		tally.Rollup(s.Outcome())
		p.builder.AddSection(section, s.Outcome(), tally)
	}

	return nil
}

// ----------------------------------------------------------------------------
// Helpers...

func isSet(b *bool) bool {
	return b != nil && *b
}

func ensurePath(path string, mod os.FileMode) error {
	dir, err := filepath.Abs(path)
	if err != nil {
		return err
	}

	_, err = os.Stat(dir)
	if err == nil || !os.IsNotExist(err) {
		return nil
	}

	if err = os.MkdirAll(dir, mod); err != nil {
		return fmt.Errorf("Fail to create popeye sanitizers dump dir: %v", err)
	}
	return nil
}

func in(list []string, member string) bool {
	if len(list) == 0 {
		return true
	}

	for _, m := range list {
		if m == member {
			return true
		}
	}

	return false
}
