// Command progen generates random programs in the subset of Teyru that Java
// has too, compiles and runs each of them with both implementations, and
// reports where the two disagree.
//
// It is the other half of the JDK differential: TestJDKDiff compares the
// programs the suite already has, which are the ones somebody thought to write
// down, and this compares programs nobody thought of -- thousands of small
// combinations of arithmetic, conversions, strings, arrays, loops, switches,
// exceptions, virtual calls and boxed identity.
//
// A program is generated from a seed, so a run is reproducible: the checked-in
// run is the first two hundred seeds, and any other range can be asked for
// locally. A disagreement is minimised by deleting the statements that are not
// needed to keep it, and the minimised program is what gets written out -- a
// test case the size of the bug is worth a hundred the size of the generator.
//
// Everything here is the Go standard library, plus this compiler's own front
// end: progen is part of the repository it tests.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	var (
		seeds    = flag.Int("seeds", 200, "how many seeds to run, from -start")
		start    = flag.Int("start", 0, "the first seed")
		jdkHome  = flag.String("jdk", os.Getenv("TEYRU_JDK"), "the JDK home to compile and run the Java with ($TEYRU_JDK)")
		outDir   = flag.String("out", "", "write the minimised difference here (a directory)")
		name     = flag.String("name", "", "the case name for -out (default progen_<seed>)")
		keep     = flag.Bool("keep", false, "keep the generated sources")
		verbose  = flag.Bool("v", false, "print each seed's program and both answers")
		oneSeed  = flag.Int("seed", -1, "run one seed and print the program instead of the summary")
		minimal  = flag.Bool("minimise", false, "minimise the first difference found and print it")
		showDiff = flag.Int("show", 3, "print this many differing seeds in full")
		skipList = flag.String("skip", "", "comma-separated statement kinds to leave out, so that the search is for differences nobody has found yet (see the summary for the kinds)")
	)
	flag.Parse()

	if *jdkHome == "" {
		fmt.Fprintln(os.Stderr, "progen: set -jdk or TEYRU_JDK to a JDK 21 home, e.g.")
		fmt.Fprintln(os.Stderr, "        TEYRU_JDK=/opt/jdk21/jdk-21.0.11+10 go run ./internal/tools/progen -seeds 200")
		os.Exit(2)
	}
	javac, err := jdkTool(*jdkHome, "javac")
	if err != nil {
		fmt.Fprintln(os.Stderr, "progen:", err)
		os.Exit(2)
	}
	javaBin, err := jdkTool(*jdkHome, "java")
	if err != nil {
		fmt.Fprintln(os.Stderr, "progen:", err)
		os.Exit(2)
	}
	r := &runner{javac: javac, java: javaBin, keep: *keep}
	skip := map[string]bool{}
	for _, name := range strings.Split(*skipList, ",") {
		if name = strings.TrimSpace(name); name != "" {
			skip[name] = true
		}
	}

	if *oneSeed >= 0 {
		p := generate(*oneSeed, "", skip)
		fmt.Println("--- " + p.cls + ".teyru")
		fmt.Print(p.render(teyru))
		fmt.Println("--- " + p.cls + ".java")
		fmt.Print(p.render(java))
		res := r.run(p)
		fmt.Printf("--- teyru: exit %d\n%s--- java: exit %d\n%s", res.teyruCode, res.teyruOut, res.javaCode, res.javaOut)
		if res.same() {
			fmt.Println("--- they agree")
			return
		}
		fmt.Println("--- they differ")
		return
	}

	agreed := 0
	differed := 0
	firstDiff := -1
	classes := map[string]int{}
	var failures []string
	for seed := *start; seed < *start+*seeds; seed++ {
		p := generate(seed, "", skip)
		res := r.run(p)
		if res.err != nil {
			// A program that does not compile on one side is a bug in the
			// generator, not a difference: say so rather than counting it.
			failures = append(failures, fmt.Sprintf("seed %d: %v", seed, res.err))
			continue
		}
		if res.same() {
			agreed++
			if *verbose {
				fmt.Printf("seed %d: agree\n", seed)
			}
			continue
		}
		differed++
		if firstDiff < 0 {
			firstDiff = seed
		}
		for _, key := range res.differingKeys() {
			classes[p.kind(key)]++
		}
		if differed <= *showDiff {
			fmt.Printf("--- seed %d differs\n%s", seed, res.report())
		}
	}

	fmt.Printf("progen: %d seeds, %d agreed, %d differed", *seeds, agreed, differed)
	if len(failures) > 0 {
		fmt.Printf(", %d did not compile (a generator bug)", len(failures))
	}
	fmt.Println()
	if differed > 0 {
		fmt.Println("differing constructs, by the statement that printed the line:")
		for _, name := range sortedCounts(classes) {
			fmt.Printf("  %5d  %s\n", classes[name], name)
		}
	}
	for _, f := range failures {
		fmt.Println("  " + f)
	}

	if *minimal && firstDiff >= 0 {
		seed := firstDiff
		p := generate(seed, "", skip)
		small := minimise(r, p)
		fmt.Printf("--- seed %d, minimised to %d of %d statements\n", seed, len(small.chunks), len(p.chunks))
		fmt.Print(small.render(teyru))
		res := r.run(small)
		fmt.Print(res.report())
		if *outDir != "" {
			caseName := *name
			if caseName == "" {
				caseName = fmt.Sprintf("progen_%d", seed)
			}
			if err := writeCase(*outDir, caseName, small, res); err != nil {
				fmt.Fprintln(os.Stderr, "progen:", err)
				os.Exit(1)
			}
			fmt.Printf("--- wrote %s/%s.{teyru,expected,java.ref}\n", *outDir, caseName)
		}
		os.Exit(1)
	}
	if differed > 0 || len(failures) > 0 {
		os.Exit(1)
	}
}

// minimal is the smallest program that still differs: the chunks that can be
// deleted without the two implementations agreeing again.
func minimise(r *runner, p *program) *program {
	for changed := true; changed; {
		changed = false
		for i := 0; i < len(p.chunks); i++ {
			candidate := p.withOut(i)
			res := r.run(candidate)
			if res.err != nil {
				// Removing the statement broke the program: it was load-bearing
				// for a declaration, whatever it printed.
				continue
			}
			if res.same() {
				continue
			}
			p = candidate
			changed = true
			i--
		}
	}
	return p
}

func jdkTool(home, name string) (string, error) {
	for _, cand := range []string{filepath.Join(home, "bin", name), filepath.Join(home, name)} {
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			return cand, nil
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("no %s in %s", name, home)
}

func sortedCounts(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// writeCase writes a minimised difference as a case of the end-to-end suite:
// the Teyru program, the Java it was generated from, and the JDK's own output
// as the expectation.
func writeCase(dir, name string, p *program, res result) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	ref := "// The Java progen generated beside this file, and ran to produce the\n" +
		"// .expected: the two implementations disagree here, which is why the case\n" +
		"// exists.\n\n" + p.render(java)
	files := []struct {
		suffix  string
		content string
	}{
		{".teyru", p.render(teyru)},
		{".expected", res.javaOut},
		{".java.ref", ref},
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, name+f.suffix), []byte(f.content), 0o644); err != nil {
			return err
		}
	}
	return nil
}
