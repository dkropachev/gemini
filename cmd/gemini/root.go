// Copyright (C) 2018 ScyllaDB

package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math/rand"
	"sync"
	"time"

	"github.com/briandowns/spinner"
	"github.com/scylladb/gemini"
	"github.com/spf13/cobra"
	"golang.org/x/net/context"
)

var (
	testClusterHost   string
	oracleClusterHost string
	schemaFile        string
	concurrency       int
	pkNumberPerThread int
	seed              int
	dropSchema        bool
	verbose           bool
	mode              string
	failFast          bool
	nonInteractive    bool
	duration          time.Duration
)

const (
	writeMode = "write"
	readMode  = "read"
	mixedMode = "mixed"
)

type Status struct {
	WriteOps    int
	WriteErrors int
	ReadOps     int
	ReadErrors  int
}

type Results interface {
	Merge(*Status) Status
	Print()
}

func interactive() bool {
	return !nonInteractive
}

type testJob func(context.Context, *sync.WaitGroup, *gemini.Schema, gemini.Table, *gemini.Session, gemini.PartitionRange, chan Status, string)

func (r *Status) Merge(sum *Status) Status {
	sum.WriteOps += r.WriteOps
	sum.WriteErrors += r.WriteErrors
	sum.ReadOps += r.ReadOps
	sum.ReadErrors += r.ReadErrors
	return *sum
}

func (r *Status) PrintResult() {
	fmt.Println("Results:")
	fmt.Printf("\twrite ops:    %v\n", r.WriteOps)
	fmt.Printf("\tread ops:     %v\n", r.ReadOps)
	fmt.Printf("\twrite errors: %v\n", r.WriteErrors)
	fmt.Printf("\tread errors:  %v\n", r.ReadErrors)
}

func (r Status) String() string {
	return fmt.Sprintf("write ops: %v | read ops: %v | write errors: %v | read errors: %v", r.WriteOps, r.ReadOps, r.WriteErrors, r.ReadErrors)
}

func readSchema(confFile string) (*gemini.Schema, error) {
	byteValue, err := ioutil.ReadFile(confFile)
	if err != nil {
		return nil, err
	}

	var shm gemini.Schema

	err = json.Unmarshal(byteValue, &shm)
	if err != nil {
		return nil, err
	}

	schemaBuilder := gemini.NewSchemaBuilder()
	schemaBuilder.Keyspace(shm.Keyspace)
	for _, tbl := range shm.Tables {
		schemaBuilder.Table(tbl)
	}
	return schemaBuilder.Build(), nil
}

func run(cmd *cobra.Command, args []string) {
	rand.Seed(int64(seed))
	fmt.Printf("Seed:                            %d\n", seed)
	fmt.Printf("Maximum duration:                %s\n", duration)
	fmt.Printf("Concurrency:                     %d\n", concurrency)
	fmt.Printf("Number of partitions per thread: %d\n", pkNumberPerThread)
	fmt.Printf("Test cluster:                    %s\n", testClusterHost)
	fmt.Printf("Oracle cluster:                  %s\n", oracleClusterHost)

	var schema *gemini.Schema
	if len(schemaFile) > 0 {
		var err error
		schema, err = readSchema(schemaFile)
		if err != nil {
			fmt.Printf("cannot create schema: %v", err)
			return
		}
	} else {
		schema = gemini.GenSchema()
	}

	jsonSchema, _ := json.MarshalIndent(schema, "", "    ")
	fmt.Printf("Schema: %v\n", string(jsonSchema))

	session := gemini.NewSession(testClusterHost, oracleClusterHost)
	defer session.Close()

	if dropSchema && mode != readMode {
		for _, stmt := range schema.GetDropSchema() {
			if verbose {
				fmt.Println(stmt)
			}
			if err := session.Mutate(stmt); err != nil {
				fmt.Printf("%v", err)
				return
			}
		}
	}
	for _, stmt := range schema.GetCreateSchema() {
		if verbose {
			fmt.Println(stmt)
		}
		if err := session.Mutate(stmt); err != nil {
			fmt.Printf("%v", err)
			return
		}
	}

	runJob(Job, schema, session, mode)
}

func runJob(f testJob, schema *gemini.Schema, s *gemini.Session, mode string) {
	c := make(chan Status)
	minRange := 0
	maxRange := pkNumberPerThread

	// Wait group for the worker goroutines.
	var workers sync.WaitGroup
	workerCtx, cancelWorkers := context.WithCancel(context.Background())
	workers.Add(len(schema.Tables) * concurrency)

	for _, table := range schema.Tables {
		for i := 0; i < concurrency; i++ {
			p := gemini.PartitionRange{
				Min:  minRange + i*maxRange,
				Max:  maxRange + i*maxRange,
				Rand: rand.New(rand.NewSource(int64(seed))),
			}
			go f(workerCtx, &workers, schema, table, s, p, c, mode)
		}
	}

	// Wait group for the reporter goroutine.
	var reporter sync.WaitGroup
	reporter.Add(1)
	reporterCtx, cancelReporter := context.WithCancel(context.Background())
	go func(d time.Duration) {
		defer reporter.Done()
		var testRes Status
		timer := time.NewTimer(d)
		var sp *spinner.Spinner = nil
		if interactive() {
			spinnerCharSet := []string{"|", "/", "-", "\\"}
			sp = spinner.New(spinnerCharSet, 1*time.Second)
			sp.Color("black")
			sp.Start()
			defer sp.Stop()
		}
		for {
			select {
			case <-timer.C:
				testRes.PrintResult()
				fmt.Println("Test run completed. Exiting.")
				cancelWorkers()
				return
			case <-reporterCtx.Done():
				testRes.PrintResult()
				return
			case res := <-c:
				testRes = res.Merge(&testRes)
				if sp != nil {
					sp.Suffix = fmt.Sprintf(" Running Gemini... %v", testRes)
				}
				if testRes.ReadErrors > 0 {
					testRes.PrintResult()
					if failFast {
						fmt.Println("Error in data validation. Exiting.")
						cancelWorkers()
						return
					}
				}
			}
		}
	}(duration)

	workers.Wait()
	cancelReporter()
	reporter.Wait()
}

func mutationJob(schema *gemini.Schema, table gemini.Table, s *gemini.Session, p gemini.PartitionRange, testStatus *Status) {
	mutateStmt, err := schema.GenMutateStmt(table, &p)
	if err != nil {
		fmt.Printf("Failed! Mutation statement generation failed: '%v'\n", err)
		testStatus.WriteErrors++
		return
	}
	mutateQuery := mutateStmt.Query
	mutateValues := mutateStmt.Values()
	if verbose {
		fmt.Printf("%s (values=%v)\n", mutateQuery, mutateValues)
	}
	testStatus.WriteOps++
	if err := s.Mutate(mutateQuery, mutateValues...); err != nil {
		fmt.Printf("Failed! Mutation '%s' (values=%v) caused an error: '%v'\n", mutateQuery, mutateValues, err)
		testStatus.WriteErrors++
	}
}

func validationJob(schema *gemini.Schema, table gemini.Table, s *gemini.Session, p gemini.PartitionRange, testStatus *Status) {
	checkStmt := schema.GenCheckStmt(table, &p)
	checkQuery := checkStmt.Query
	checkValues := checkStmt.Values()
	if verbose {
		fmt.Printf("%s (values=%v)\n", checkQuery, checkValues)
	}
	err := s.Check(table, checkQuery, checkValues...)
	if err == nil {
		testStatus.ReadOps++
	} else {
		if err != gemini.ErrReadNoDataReturned {
			fmt.Printf("Failed! Check '%s' (values=%v)\n%s\n", checkQuery, checkValues, err)
			testStatus.ReadErrors++
		}
	}
}

func Job(ctx context.Context, wg *sync.WaitGroup, schema *gemini.Schema, table gemini.Table, s *gemini.Session, p gemini.PartitionRange, c chan Status, mode string) {
	defer wg.Done()
	testStatus := Status{}

	var i int
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		switch mode {
		case writeMode:
			mutationJob(schema, table, s, p, &testStatus)
		case readMode:
			validationJob(schema, table, s, p, &testStatus)
		default:
			ind := p.Rand.Intn(100000) % 2
			if ind == 0 {
				mutationJob(schema, table, s, p, &testStatus)
			} else {
				validationJob(schema, table, s, p, &testStatus)
			}
		}

		if i%1000 == 0 {
			c <- testStatus
			testStatus = Status{}
		}
		if failFast && testStatus.ReadErrors > 0 {
			break
		}
		i++
	}

	c <- testStatus
}

var rootCmd = &cobra.Command{
	Use:   "gemini",
	Short: "Gemini is an automatic random testing tool for Scylla.",
	Run:   run,
}

func Execute() {
}

func init() {

	rootCmd.Version = version + ", commit " + commit + ", date " + date
	rootCmd.Flags().StringVarP(&testClusterHost, "test-cluster", "t", "", "Host name of the test cluster that is system under test")
	rootCmd.MarkFlagRequired("test-cluster")
	rootCmd.Flags().StringVarP(&oracleClusterHost, "oracle-cluster", "o", "", "Host name of the oracle cluster that provides correct answers")
	rootCmd.MarkFlagRequired("oracle-cluster")
	rootCmd.Flags().StringVarP(&schemaFile, "schema", "", "", "Schema JSON config file")
	rootCmd.Flags().StringVarP(&mode, "mode", "m", mixedMode, "Query operation mode. Mode options: write, read, mixed (default)")
	rootCmd.Flags().IntVarP(&concurrency, "concurrency", "c", 10, "Number of threads per table to run concurrently")
	rootCmd.Flags().IntVarP(&pkNumberPerThread, "max-pk-per-thread", "p", 50, "Maximum number of partition keys per thread")
	rootCmd.Flags().IntVarP(&seed, "seed", "s", 1, "PRNG seed value")
	rootCmd.Flags().BoolVarP(&dropSchema, "drop-schema", "d", false, "Drop schema before starting tests run")
	rootCmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "Verbose output during test run")
	rootCmd.Flags().BoolVarP(&failFast, "fail-fast", "f", false, "Stop on the first failure")
	rootCmd.Flags().BoolVarP(&nonInteractive, "non-interactive", "", false, "Run in non-interactive mode (disable progress indicator)")
	rootCmd.Flags().DurationVarP(&duration, "duration", "", 30*time.Second, "")
}
