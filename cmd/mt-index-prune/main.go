package main

import (
	"flag"
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/grafana/metrictank/conf"
	"github.com/grafana/metrictank/idx/cassandra"
	"github.com/grafana/metrictank/idx/memory"
	"github.com/grafana/metrictank/logger"
	"github.com/raintank/schema"
	log "github.com/sirupsen/logrus"
)

func init() {
	formatter := &logger.TextFormatter{}
	formatter.TimestampFormat = "2006-01-02 15:04:05.000"
	log.SetFormatter(formatter)
	log.SetLevel(log.InfoLevel)
}

func perror(err error) {
	if err != nil {
		log.Fatal(err.Error())
	}
}

type counters struct {
	total      int
	active     int
	deprecated int
}

func (c *counters) PrintCounters() {
	fmt.Println(fmt.Sprintf("Total analyzed defs: %d", c.total))
	fmt.Println(fmt.Sprintf("Active defs:         %d", c.active))
	fmt.Println(fmt.Sprintf("Deprecated defs:     %d", c.deprecated))
}

func main() {
	var noDryRun, verbose bool
	var partitionCount int
	var indexRulesFile string
	globalFlags := flag.NewFlagSet("global config flags", flag.ExitOnError)
	globalFlags.BoolVar(&noDryRun, "no-dry-run", false, "do not only plan and print what to do, but also execute it")
	globalFlags.BoolVar(&verbose, "verbose", false, "print every metric name that gets archived")
	globalFlags.IntVar(&partitionCount, "partition-count", 8, "the number of partitions in existence")
	globalFlags.StringVar(&indexRulesFile, "index-rules-file", "/etc/metrictank/index-rules.conf", "name of file which defines the max-stale times")
	cassFlags := cassandra.ConfigSetup()

	flag.Usage = func() {
		fmt.Println("mt-index-prune")
		fmt.Println()
		fmt.Println("Retrieves a metrictank index and moves all deprecated entries into an archive table")
		fmt.Println()
		fmt.Printf("Usage:\n\n")
		fmt.Printf("  mt-index-prune [global config flags] <idxtype> [idx config flags]\n\n")
		fmt.Printf("global config flags:\n\n")
		globalFlags.PrintDefaults()
		fmt.Println()
		fmt.Printf("idxtype: only 'cass' supported for now\n\n")
		fmt.Printf("cass config flags:\n\n")
		cassFlags.PrintDefaults()
		fmt.Println()
		fmt.Println()
		fmt.Println("EXAMPLES:")
		fmt.Println("mt-index-prune --verbose --partition-count 128 cass -hosts cassandra:9042")
	}

	if len(os.Args) == 2 && (os.Args[1] == "-h" || os.Args[1] == "--help") {
		flag.Usage()
		os.Exit(0)
	}

	if len(os.Args) < 2 {
		flag.Usage()
		os.Exit(-1)
	}

	var cassI int
	for i, v := range os.Args {
		if v == "cass" {
			cassI = i
		}
	}
	if cassI == 0 {
		log.Println("only indextype 'cass' supported")
		flag.Usage()
		os.Exit(1)
	}
	globalFlags.Parse(os.Args[1:cassI])

	indexRules, err := conf.ReadIndexRules(indexRulesFile)
	if os.IsNotExist(err) {
		log.Fatalf("Index-rules.conf file %s does not exist; using defaults", indexRulesFile)
	}
	now := time.Now()
	cutoffs := indexRules.Cutoffs(now)

	cassFlags.Parse(os.Args[cassI+1:])
	cassandra.CliConfig.Enabled = true

	cassIdx := cassandra.New(cassandra.CliConfig)
	err = cassIdx.InitBare()
	perror(err)

	// we don't want to filter any metric definitions during the loading
	// so MaxStale is set to 0
	memory.IndexRules = conf.IndexRules{
		Rules: nil,
		Default: conf.IndexRule{
			Name:     "default",
			Pattern:  regexp.MustCompile(""),
			MaxStale: 0,
		},
	}

	defCounters := counters{}
	defs := make([]schema.MetricDefinition, 0)
	deprecatedDefs := make([]schema.MetricDefinition, 0)
	for partition := int32(0); partition < int32(partitionCount); partition++ {
		defsByNameWithTags := make(map[string][]schema.MetricDefinition)
		defs = cassIdx.LoadPartitions([]int32{partition}, defs, now)
		defCounters.total += len(defs)
		for _, def := range defs {
			name := def.NameWithTags()
			defsByNameWithTags[name] = append(defsByNameWithTags[name], def)
		}

		for name, defs := range defsByNameWithTags {
			// find the latest LastUpdate ts
			latest := int64(0)
			for _, def := range defs {
				if def.LastUpdate > latest {
					latest = def.LastUpdate
				}
			}

			irId, _ := indexRules.Match(name)
			if latest < cutoffs[irId] {
				for _, def := range defs {
					deprecatedDefs = append(deprecatedDefs, def)
				}
				defCounters.deprecated += len(defs)

				if verbose {
					fmt.Println(fmt.Sprintf("Metric is deprecated: %s", name))
				}
			} else {
				defCounters.active += len(defs)

				if verbose {
					fmt.Println(fmt.Sprintf("Metric is active: %s", name))
				}
			}
		}

		if noDryRun {
			err = cassIdx.ArchiveDefs(deprecatedDefs)
			if err != nil {
				perror(fmt.Errorf("Failed to archive defs: %s", err.Error()))
			}
		}

		defs = defs[:0]
		deprecatedDefs = deprecatedDefs[:0]
	}

	defCounters.PrintCounters()
}
