// Copyright 2017 Google Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package build

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sync"
	"text/template"

	"android/soong/elf"
	"android/soong/ui/metrics"
)

// SetupOutDir ensures the out directory exists, and has the proper files to
// prevent kati from recursing into it.
func SetupOutDir(ctx Context, config Config) {
	ensureEmptyFileExists(ctx, filepath.Join(config.OutDir(), "Android.mk"))
	ensureEmptyFileExists(ctx, filepath.Join(config.OutDir(), "CleanSpec.mk"))
	ensureDirectoriesExist(ctx, config.SoongOutDir())
	ensureDirectoriesExist(ctx, filepath.Join(config.SoongOutDir(), "action_sandboxing_workdir"))

	// The ninja_build file is used by our buildbots to understand that the output
	// can be parsed as ninja output.
	ensureEmptyFileExists(ctx, filepath.Join(config.OutDir(), "ninja_build"))
	ensureEmptyFileExists(ctx, filepath.Join(config.OutDir(), ".out-dir"))

	if buildDateTimeFile, ok := config.environ.Get("BUILD_DATETIME_FILE"); ok {
		err := os.WriteFile(buildDateTimeFile, []byte(config.buildDateTime), 0666) // a+rw
		if err != nil {
			ctx.Fatalln("Failed to write BUILD_DATETIME to file:", err)
		}
	} else {
		ctx.Fatalln("Missing BUILD_DATETIME_FILE")
	}

	writeValueIfChanged(ctx, filepath.Join(config.OutDir(), "file_name_tag.txt"), config.DistFileNameTag())
	// Write the build number to a file so it can be read back in
	// without changing the command line every time.  Avoids rebuilds
	// when using ninja.
	writeValueIfChanged(ctx, filepath.Join(config.SoongOutDir(), "build_number.txt"), config.BuildNumber())

	hostname, ok := config.environ.Get("BUILD_HOSTNAME")
	if !ok {
		var err error
		hostname, err = os.Hostname()
		if err != nil {
			ctx.Println("Failed to read hostname:", err)
			hostname = "unknown"
		}
	}
	writeValueIfChanged(ctx, filepath.Join(config.SoongOutDir(), "build_hostname.txt"), hostname)

	buildTargetName, ok := config.environ.Get("BUILD_TARGET_NAME")
	if targetProduct, err := config.TargetProductOrErr(); !ok && err == nil {
		buildTargetName = targetProduct
	}

	buildUUID := buildUUID(buildTargetName, config.BuildNumber())
	buildUUIDFile := config.BuildUUIDFile()
	writeValueIfChanged(ctx, buildUUIDFile, buildUUID)
	distFileToFile(ctx, config, buildUUIDFile, "BUILD_UUID")
}

// Compute a UUID based on the hash of the build name and the build number.
func buildUUID(targetName, number string) string {
	h := sha256.New()
	must := func(n int, err error) {
		if err != nil {
			panic(err)
		}
	}
	must(io.WriteString(h, targetName))
	must(io.WriteString(h, number))
	return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(h.Sum(nil))
}

// SetupTempDir makes sure config.TempDir() exists and is empty.
func SetupTempDir(ctx Context, config Config) {
	ensureEmptyDirectoriesExist(ctx, config.TempDir())
}

var combinedBuildNinjaTemplate = template.Must(template.New("combined").Parse(`
builddir = {{.OutDir}}
{{if .UseRemoteBuild }}pool local_pool
 depth = {{.Parallel}}
{{end -}}
pool highmem_pool
 depth = {{.HighmemParallel}}
{{if and (not .SkipKatiNinja) .HasKatiSuffix}}
subninja {{.KatiBuildNinjaFile}}
subninja {{.KatiPackageNinjaFile}}
{{else}}
dist={{if .Dist}}dist{{else}}nodist{{end}}
distdir={{.DistDir}}
distFileNameTag={{.DistFileNameTag}}
{{end -}}
subninja {{.SoongNinjaFile}}
`))

func createCombinedBuildNinjaFile(ctx Context, config Config) {
	// If we're in SkipKati mode but want to run kati ninja, skip creating this file if it already exists
	if config.SkipKati() && !config.SkipKatiNinja() {
		if _, err := os.Stat(config.CombinedNinjaFile()); err == nil || !os.IsNotExist(err) {
			return
		}
	}

	file, err := os.Create(config.CombinedNinjaFile())
	if err != nil {
		ctx.Fatalln("Failed to create combined ninja file:", err)
	}
	defer file.Close()

	if err := combinedBuildNinjaTemplate.Execute(file, config); err != nil {
		ctx.Fatalln("Failed to write combined ninja file:", err)
	}
}

// These are bitmasks which can be used to check whether various flags are set
const (
	_ = iota
	// Whether to run the kati config step.
	RunProductConfig = 1 << iota
	// Whether to run soong to generate a ninja file.
	RunSoong = 1 << iota
	// Whether to run kati to generate a ninja file.
	RunKati = 1 << iota
	// Whether to include the kati-generated ninja file in the combined ninja.
	RunKatiNinja = 1 << iota
	// Whether to run ninja on the combined ninja.
	RunNinja       = 1 << iota
	RunDistActions = 1 << iota
	RunBuildTests  = 1 << iota
)

// checkProblematicFiles fails the build if existing Android.mk or CleanSpec.mk files are found at the root of the tree.
func checkProblematicFiles(ctx Context) {
	files := []string{"Android.mk", "CleanSpec.mk"}
	for _, file := range files {
		if _, err := os.Stat(file); !os.IsNotExist(err) {
			absolute := absPath(ctx, file)
			ctx.Printf("Found %s in tree root. This file needs to be removed to build.\n", file)
			ctx.Fatalf("    rm %s\n", absolute)
		}
	}
}

// checkCaseSensitivity issues a warning if a case-insensitive file system is being used.
func checkCaseSensitivity(ctx Context, config Config) {
	outDir := config.OutDir()
	lowerCase := filepath.Join(outDir, "casecheck.txt")
	upperCase := filepath.Join(outDir, "CaseCheck.txt")
	lowerData := "a"
	upperData := "B"

	if err := ioutil.WriteFile(lowerCase, []byte(lowerData), 0666); err != nil { // a+rw
		ctx.Fatalln("Failed to check case sensitivity:", err)
	}

	if err := ioutil.WriteFile(upperCase, []byte(upperData), 0666); err != nil { // a+rw
		ctx.Fatalln("Failed to check case sensitivity:", err)
	}

	res, err := ioutil.ReadFile(lowerCase)
	if err != nil {
		ctx.Fatalln("Failed to check case sensitivity:", err)
	}

	if string(res) != lowerData {
		ctx.Println("************************************************************")
		ctx.Println("You are building on a case-insensitive filesystem.")
		ctx.Println("Please move your source tree to a case-sensitive filesystem.")
		ctx.Println("************************************************************")
		ctx.Fatalln("Case-insensitive filesystems not supported")
	}
}

// help prints a help/usage message, via the build/make/help.sh script.
func help(ctx Context, config Config) {
	cmd := Command(ctx, config, nil, "help.sh", "build/make/help.sh")
	cmd.Sandbox = dumpvarsSandbox
	cmd.RunAndPrintOrFatal()
}

// checkRAM warns if there probably isn't enough RAM to complete a build.
func checkRAM(ctx Context, config Config) {
	if totalRAM := config.TotalRAM(); totalRAM != 0 {
		ram := float32(totalRAM) / (1024 * 1024 * 1024)
		ctx.Verbosef("Total RAM: %.3vGB", ram)

		if ram <= 16 {
			ctx.Println("************************************************************")
			ctx.Printf("You are building on a machine with %.3vGB of RAM\n", ram)
			ctx.Println("")
			ctx.Println("The minimum required amount of free memory is around 16GB,")
			ctx.Println("and even with that, some configurations may not work.")
			ctx.Println("")
			ctx.Println("If you run into segfaults or other errors, try reducing your")
			ctx.Println("-j value.")
			ctx.Println("************************************************************")
		} else if ram <= float32(config.Parallel()) {
			// Want at least 1GB of RAM per job.
			ctx.Printf("Warning: high -j%d count compared to %.3vGB of RAM", config.Parallel(), ram)
			ctx.Println("If you run into segfaults or other errors, try a lower -j value")
		}
	}
}

func abfsBuildStarted(ctx Context, config Config) {
	abfsBox := config.PrebuiltBuildTool("abfsbox")
	cmdArgs := []string{"build-started", "--"}
	cmdArgs = append(cmdArgs, config.Arguments()...)
	cmd := Command(ctx, config, nil, "abfsbox", abfsBox, cmdArgs...)
	cmd.Sandbox = noSandbox
	cmd.RunAndPrintOrFatal()
}

func abfsBuildFinished(ctx Context, config Config, finished bool) {
	var errMsg string
	if !finished {
		errMsg = "build was interrupted"
	}
	abfsBox := config.PrebuiltBuildTool("abfsbox")
	cmdArgs := []string{"build-finished", "-e", errMsg, "--"}
	cmdArgs = append(cmdArgs, config.Arguments()...)
	cmd := Command(ctx, config, nil, "abfsbox", abfsBox, cmdArgs...)
	cmd.RunAndPrintOrFatal()
}

// Build the tree. Various flags in `config` govern which components of
// the build to run.
func Build(ctx Context, config Config) {
	done := false
	ctx.Verboseln("Starting build with args:", config.Arguments())
	ctx.Verboseln("Environment:", config.Environment().Environ())

	e := ctx.BeginTrace(metrics.Total, "total")
	defer e.End()

	if config.UseABFS() {
		abfsBuildStarted(ctx, config)
		defer func() {
			abfsBuildFinished(ctx, config, done)
		}()
	}

	if inList("help", config.Arguments()) {
		help(ctx, config)
		return
	}

	// Make sure that no other Soong process is running with the same output directory
	buildLock := BecomeSingletonOrFail(ctx, config)
	defer buildLock.Unlock()

	logArgsOtherThan := func(specialTargets ...string) {
		var ignored []string
		for _, a := range config.Arguments() {
			if !inList(a, specialTargets) {
				ignored = append(ignored, a)
			}
		}
		if len(ignored) > 0 {
			ctx.Printf("ignoring arguments %q", ignored)
		}
	}

	if inList("clean", config.Arguments()) || inList("clobber", config.Arguments()) {
		logArgsOtherThan("clean", "clobber")
		clean(ctx, config)
		return
	}

	defer waitForDist(ctx)

	// checkProblematicFiles aborts the build if Android.mk or CleanSpec.mk are found at the root of the tree.
	checkProblematicFiles(ctx)

	checkRAM(ctx, config)

	SetupOutDir(ctx, config)
	SetupTempDir(ctx, config)

	// checkCaseSensitivity issues a warning if a case-insensitive file system is being used.
	checkCaseSensitivity(ctx, config)

	SetupPath(ctx, config)
	mapsCh := QueryEarlyReleaseConfig(ctx, config)

	what := evaluateWhatToRun(config, ctx.Verboseln)

	rbeCh := make(chan bool)
	var rbePanic any
	if config.StartReproxy() {
		cleanupRBELogsDir(ctx, config)
		checkRBERequirements(ctx, config)
		go func() {
			defer func() {
				rbePanic = recover()
				close(rbeCh)
			}()
			startReproxy(ctx, config)
		}()
		defer DumpRBEMetrics(ctx, config, filepath.Join(config.LogsDir(), "rbe_metrics.pb"))
	} else if config.StartRBEproxy() {
		cleanupRBELogsDir(ctx, config)
		checkRBERequirements(ctx, config)
		go func() {
			defer func() {
				rbePanic = recover()
				close(rbeCh)
			}()
			startRBEproxy(ctx, config)
		}()
		defer DumpRBEMetrics(ctx, config, filepath.Join(config.LogsDir(), "rbe_metrics.pb"))
	} else {
		close(rbeCh)
	}

	if config.RunCIPDProxyServer() && shouldRunCIPDProxy(ctx, config) {
		cipdProxy := startCIPDProxyServer(ctx, config)
		defer cipdProxy.Stop(ctx)
	}

	CollectEarlyReleaseConfig(ctx, config, mapsCh)
	if what&RunProductConfig != 0 {
		runMakeProductConfig(ctx, config)

		// Re-evaluate what to run because there are product variables that control how
		// soong and make are run.
		what = evaluateWhatToRun(config, ctx.Verboseln)
	}

	// Everything below here depends on product config.

	// Write SOONG_USE_PARTIAL_COMPILE so it can be sourced by rules that use it.
	shFile := config.DeviceUsePartialCompile()
	ensureDirectoriesExist(ctx, filepath.Dir(shFile))
	value, _ := config.environ.Get("SOONG_USE_PARTIAL_COMPILE")
	writeValueIfChanged(ctx, shFile, fmt.Sprintf("\nexport SOONG_USE_PARTIAL_COMPILE=%s\n", value))

	if inList("installclean", config.Arguments()) ||
		inList("install-clean", config.Arguments()) {
		logArgsOtherThan("installclean", "install-clean")
		installClean(ctx, config)
		ctx.Println("Deleted images and staging directories.")
		return
	}

	if inList("dataclean", config.Arguments()) ||
		inList("data-clean", config.Arguments()) {
		logArgsOtherThan("dataclean", "data-clean")
		dataClean(ctx, config)
		ctx.Println("Deleted data files.")
		return
	} else if inList("deviceclean", config.Arguments()) {
		deviceClean(ctx, config, what)
		return
	}

	// Still generate the kati suffix in soong-only builds because soong-only still uses kati for
	// the packaging step. Also, the kati suffix is used for the combined ninja file.
	genKatiSuffix(ctx, config)

	if what&RunSoong != 0 {
		runSoong(ctx, config, what&RunBuildTests != 0)
	}

	if what&RunKati != 0 {
		runKatiCleanSpec(ctx, config)
		runKatiBuild(ctx, config)
		runKatiPackage(ctx, config)

	} else if what&RunKatiNinja != 0 {
		// Load last Kati Suffix if it exists
		if katiSuffix, err := os.ReadFile(config.LastKatiSuffixFile()); err == nil {
			ctx.Verboseln("Loaded previous kati config:", string(katiSuffix))
			config.SetKatiSuffix(string(katiSuffix))
		}
	}

	os.WriteFile(config.LastKatiSuffixFile(), []byte(config.KatiSuffix()), 0666) // a+rw

	// Write combined ninja file
	createCombinedBuildNinjaFile(ctx, config)

	distGzipFile(ctx, config, config.CombinedNinjaFile(), "soong_ui")

	if what&RunBuildTests != 0 {
		testForDanglingRules(ctx, config)
	}

	<-rbeCh
	if rbePanic != nil {
		// If there was a ctx.Fatal in startReproxy, rethrow it.
		panic(rbePanic)
	}

	if what&RunNinja != 0 {
		if what&RunKati != 0 {
			installCleanIfNecessary(ctx, config)
		}
		partialCompileCleanIfNecessary(ctx, config)
		runNinjaForBuild(ctx, config)
		updateBuildIdDir(ctx, config)

		runUpdateApi(ctx, config)
		runUpdateAidlApi(ctx, config)
		createCompDbSymlink(ctx, config)
	}

	if what&RunDistActions != 0 {
		runDistActions(ctx, config)
	}
	done = true
}

func createCompDbSymlink(ctx Context, config Config) {
	if finalLinkDir, ok := config.environ.Get("SOONG_LINK_COMPDB_TO"); ok && finalLinkDir != "" {
		finalLinkPath := filepath.Join(finalLinkDir, "compile_commands.json")
		os.Remove(finalLinkPath)
		compDBFilePath := filepath.Join(config.SoongOutDir(), "development/ide/compdb/compile_commands.json")
		if err := os.Symlink(compDBFilePath, finalLinkPath); err != nil {
			ctx.Printf("Unable to symlink %s to %s: %s", compDBFilePath, finalLinkPath, err)
		}
	}
}

func updateBuildIdDir(ctx Context, config Config) {
	e := ctx.BeginTrace(metrics.RunShutdownTool, "update_build_id_dir")
	defer e.End()

	symbolsDir := filepath.Join(config.ProductOut(), "symbols")
	if err := elf.UpdateBuildIdDir(symbolsDir); err != nil {
		ctx.Printf("failed to update %s/.build-id: %v", symbolsDir, err)
	}
}

func evaluateWhatToRun(config Config, verboseln func(v ...interface{})) int {
	//evaluate what to run
	what := 0
	if config.Checkbuild() {
		what |= RunBuildTests
	}
	if value, ok := config.environ.Get("RUN_BUILD_TESTS"); ok && value == "true" {
		what |= RunBuildTests
	}
	if !config.SkipConfig() {
		what |= RunProductConfig
	} else {
		verboseln("Skipping Config as requested")
	}
	if !config.SkipSoong() {
		what |= RunSoong
	} else {
		verboseln("Skipping use of Soong as requested")
	}
	if !config.SkipKati() {
		what |= RunKati
	} else {
		verboseln("Skipping Kati as requested")
	}
	if !config.SkipKatiNinja() {
		what |= RunKatiNinja
	} else {
		verboseln("Skipping use of Kati ninja as requested")
	}
	if !config.SkipNinja() {
		what |= RunNinja
	} else {
		verboseln("Skipping Ninja as requested")
	}

	if !config.SoongBuildInvocationNeeded() {
		// This means that the output of soong_build is not needed and thus it would
		// run unnecessarily. In addition, if this code wasn't there invocations
		// with only special-cased target names would result in
		// passing Ninja the empty target list and it would then build the default
		// targets which is not what the user asked for.
		what = what &^ RunNinja
		what = what &^ RunKati
	}

	if config.Dist() {
		what |= RunDistActions
	}

	return what
}

var distWaitGroup sync.WaitGroup

// waitForDist waits for all backgrounded distGzipFile and distFile writes to finish
func waitForDist(ctx Context) {
	e := ctx.BeginTrace("soong_ui", "dist")
	defer e.End()

	distWaitGroup.Wait()
}

// distGzipFile writes a compressed copy of src to the distDir if dist is enabled.  Failures
// are printed but non-fatal. Uses the distWaitGroup func for backgrounding (optimization).
func distGzipFile(ctx Context, config Config, src string, subDirs ...string) {
	if !config.Dist() {
		return
	}

	subDir := filepath.Join(subDirs...)
	destDir := filepath.Join(config.RealDistDir(), subDir)

	if err := os.MkdirAll(destDir, 0777); err != nil { // a+rwx
		ctx.Printf("failed to mkdir %s: %s", destDir, err.Error())
	}

	distWaitGroup.Add(1)
	go func() {
		defer distWaitGroup.Done()
		if err := gzipFileToDir(src, destDir); err != nil {
			ctx.Printf("failed to dist %s: %s", filepath.Base(src), err.Error())
		}
	}()
}

// distFile writes a copy of src to the distDir if dist is enabled.  Failures are printed but
// non-fatal. Uses the distWaitGroup func for backgrounding (optimization).
func distFile(ctx Context, config Config, src string, subDirs ...string) {
	if !config.Dist() {
		return
	}

	subDir := filepath.Join(subDirs...)
	distFileToFile(ctx, config, src, subDir, filepath.Base(src))
}

// distFileToFile writes a copy of src to dest in distDir if dist is enabled.  Failures are printed but
// non-fatal. Uses the distWaitGroup func for backgrounding (optimization).
func distFileToFile(ctx Context, config Config, src string, destParts ...string) {
	if !config.Dist() {
		return
	}

	dest := filepath.Join(config.RealDistDir(), filepath.Join(destParts...))
	destDir := filepath.Dir(dest)

	if err := os.MkdirAll(destDir, 0777); err != nil { // a+rwx
		ctx.Printf("failed to mkdir %s: %s", destDir, err.Error())
	}

	distWaitGroup.Add(1)
	go func() {
		defer distWaitGroup.Done()
		if _, err := copyFile(src, dest); err != nil {
			ctx.Printf("failed to dist %s: %s", filepath.Base(src), err.Error())
		}
	}()
}

// Actions to run on every build where 'dist' is in the actions.
// Be careful, anything added here slows down EVERY CI build
func runDistActions(ctx Context, config Config) {
	// Always dist the build flags used in this build.
	if product, err := config.TargetProductOrErr(); err == nil {
		buildFlagsDir := filepath.Join(config.DistDir(), "build_flags")
		ensureDirectoriesExist(ctx, buildFlagsDir)
		flagsFile := filepath.Join(config.SoongOutDir(), "release-config", fmt.Sprintf("release_config-%s.vars", product))
		flagsData, err := os.ReadFile(flagsFile)
		if err != nil {
			ctx.Printf("failed to read %s: %v", flagsFile, err)
			return
		}
		distFlagsFile := filepath.Join(buildFlagsDir, filepath.Base(flagsFile))
		os.WriteFile(distFlagsFile, flagsData, 0666)
	}

	runStagingSnapshot(ctx, config)
}
