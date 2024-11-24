// Package generator acts as a prisma generator
package generator

import (
	"bytes"
	"embed"
	"fmt"
	"go/format"
	"os"
	"path"
	"strings"
	"text/template"

	"github.com/steebchen/prisma-client-go/binaries"
	"github.com/steebchen/prisma-client-go/binaries/bindata"
	"github.com/steebchen/prisma-client-go/binaries/platform"
	"github.com/steebchen/prisma-client-go/logger"
)

const DefaultPackageName = "db"

func addDefaults(input *Root) {
	if input.Generator.Config.Package == "" {
		input.Generator.Config.Package = DefaultPackageName
	}

	if binaryTargets := os.Getenv("PRISMA_CLI_BINARY_TARGETS"); binaryTargets != "" {
		s := strings.Split(binaryTargets, ",")
		var targets []BinaryTarget
		for _, t := range s {
			targets = append(targets, BinaryTarget{Value: t})
		}
		input.Generator.BinaryTargets = targets
		logger.Debug.Printf("overriding binary targets: %+v", targets)
	}
}

// Run invokes the generator, which builds the templates and writes to the specified output file.
func Run(input *Root) error {
	addDefaults(input)

	if input.Version != binaries.EngineVersion {
		fmt.Printf("\nwarning: prisma CLI version mismatch detected. CLI version: %s, internal version: %s (%s); please see https://github.com/steebchen/prisma-client-go/issues/1099 for details\n\n", input.Version, binaries.EngineVersion, binaries.PrismaVersion)
	}

	if input.Generator.Config.DisableGitignore != "true" && input.Generator.Config.DisableGoBinaries != "true" {
		logger.Debug.Printf("writing gitignore file")
		// generate a gitignore into the folder
		var gitignore = "# gitignore generated by Prisma Client Go. DO NOT EDIT.\n*_gen.go\n"
		if err := os.MkdirAll(input.Generator.Output.Value, os.ModePerm); err != nil {
			return fmt.Errorf("could not create output directory: %w", err)
		}
		if err := os.WriteFile(path.Join(input.Generator.Output.Value, ".gitignore"), []byte(gitignore), 0644); err != nil {
			return fmt.Errorf("could not write .gitignore: %w", err)
		}
	}

	if err := generateClient(input); err != nil {
		return fmt.Errorf("generate client: %w", err)
	}

	if err := generateBinaries(input); err != nil {
		return fmt.Errorf("generate binaries: %w", err)
	}

	return nil
}

//go:embed templates/*.gotpl templates/actions/*.gotpl
var templateFS embed.FS

func generateClient(input *Root) error {
	var buf bytes.Buffer

	// manually define the order of the templates for consistent output
	files := []string{
		"_header",
		"client",
		"enums",
		"errors",
		"fields",
		"mock",
		"models",
		"query",
		"actions/actions",
		"actions/create",
		"actions/find",
		"actions/transaction",
		"actions/upsert",
		"actions/raw",
	}

	var templates []*template.Template
	for _, file := range files {
		t, err := template.ParseFS(templateFS, "templates/"+file+".gotpl")
		if err != nil {
			return fmt.Errorf("could not parse template fs: %w", err)
		}
		templates = append(templates, t)
	}

	// Then process all remaining templates
	for _, tpl := range templates {
		buf.Write([]byte(fmt.Sprintf("// --- template %s ---\n", tpl.Name())))

		if err := tpl.Execute(&buf, input); err != nil {
			return fmt.Errorf("could not write template file %s: %w", tpl.Name(), err)
		}

		if _, err := format.Source(buf.Bytes()); err != nil {
			return fmt.Errorf("could not format source %s from file %s %s: %w", buf.String(), tpl.Name(), input.SchemaPath, err)
		}
	}

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return fmt.Errorf("could not format final source: %w", err)
	}

	output := input.Generator.Output.Value

	if strings.HasSuffix(output, ".go") {
		return fmt.Errorf("generator output should be a directory")
	}

	if err := os.MkdirAll(output, os.ModePerm); err != nil {
		return fmt.Errorf("could not run MkdirAll on path %s: %w", output, err)
	}

	// TODO make this configurable
	outFile := path.Join(output, "db_gen.go")
	if err := os.WriteFile(outFile, formatted, 0644); err != nil {
		return fmt.Errorf("could not write template data to file writer %s: %w", outFile, err)
	}

	return nil
}

func generateBinaries(input *Root) error {
	if input.Generator.Config.DisableGoBinaries == "true" {
		return nil
	}

	if input.GetEngineType() == "dataproxy" {
		logger.Debug.Printf("using data proxy; not fetching any engines")
		return nil
	}

	var targets []string
	var isNonLinux bool

	logger.Debug.Printf("defined binary targets: %v", input.Generator.BinaryTargets)

	for _, target := range input.Generator.BinaryTargets {
		targets = append(targets, target.Value)
		if target.Value == "darwin" || target.Value == "windows" {
			isNonLinux = true
		}
	}

	// add native by default if native binary is darwin or linux
	// this prevents conflicts when building on linux
	if isNonLinux || len(targets) == 0 {
		targets = add(targets, "native")
	}

	logger.Debug.Printf("final binary targets: %v", targets)

	// TODO refactor
	for _, name := range targets {
		if name == "native" {
			name = platform.BinaryPlatformNameStatic()
			logger.Debug.Printf("swapping 'native' binary target with '%s'", name)
		}

		name = TransformBinaryTarget(name)

		// first, ensure they are actually downloaded
		if err := binaries.FetchEngine(binaries.GlobalCacheDir(), "query-engine", name); err != nil {
			return fmt.Errorf("failed fetching binaries: %w", err)
		}
	}

	if err := generateQueryEngineFiles(targets, input.Generator.Config.Package.String(), input.Generator.Output.Value); err != nil {
		return fmt.Errorf("could not write template data: %w", err)
	}

	return nil
}

func generateQueryEngineFiles(binaryTargets []string, pkg, outputDir string) error {
	for _, name := range binaryTargets {
		if name == "native" {
			name = platform.BinaryPlatformNameStatic()
		}

		info := platform.MapBinaryTarget(name)

		name = TransformBinaryTarget(name)

		enginePath := binaries.GetEnginePath(binaries.GlobalCacheDir(), "query-engine", name)

		filename := fmt.Sprintf("query-engine-%s_gen.go", name)
		to := path.Join(outputDir, filename)

		// TODO check if already exists, but make sure version matches
		if err := bindata.WriteFile(name, pkg, enginePath, to, info); err != nil {
			return fmt.Errorf("generate write go file: %w", err)
		}

		logger.Debug.Printf("write go file at %s", filename)
	}

	return nil
}

func add(list []string, item string) []string {
	keys := make(map[string]bool)
	if _, ok := keys[item]; !ok {
		keys[item] = true
		list = append(list, item)
	}
	return list
}

func TransformBinaryTarget(name string) string {
	// TODO this is a temp fix as the exact alpine libraries are not working
	if name == "linux" || strings.Contains(name, "musl") {
		name = "linux-static-" + platform.Arch()
		logger.Debug.Printf("overriding binary name with '%s' due to linux or musl", name)
	}
	return name
}
