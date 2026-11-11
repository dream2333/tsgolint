package utils

import (
	"runtime"
	"sync"

	"github.com/microsoft/TypeScript/tsc/shim/core"
	"github.com/microsoft/TypeScript/tsc/shim/project"
	"github.com/microsoft/TypeScript/tsc/shim/tsoptions"
	"github.com/microsoft/TypeScript/tsc/shim/tspath"
	"github.com/microsoft/TypeScript/tsc/shim/vfs"
	"github.com/typescript-eslint/tsgolint/internal/collections"
)

type TsConfigResolver struct {
	fs                        vfs.FS
	currentDirectory          string
	currentDirectoryPath      tspath.RootedDirectoryPath
	configFileRegistryBuilder *project.ConfigFileRegistryBuilder
}

func NewTsConfigResolver(fs vfs.FS, currentDirectory string) *TsConfigResolver {
	currentDirectoryPath := tspath.RootedDirectoryPathFromAbsolute(currentDirectory)
	return &TsConfigResolver{
		fs:                   fs,
		currentDirectory:     currentDirectory,
		currentDirectoryPath: currentDirectoryPath,
		configFileRegistryBuilder: project.NewConfigFileRegistryBuilder(
			false,
			project.TsGoLintNewSnapshotFSBuilder(fs), func(tspath.PathKey) bool { return false }, &project.ConfigFileRegistry{}, project.NewExtendedConfigCache(), 0, &project.SessionOptions{
				CurrentDirectory: currentDirectoryPath,
			}, "", nil),
	}
}

// Finds the tsconfig.json that governs the given file
// Reference: `findOrCreateDefaultConfiguredProjectForOpenScriptInfo` typescript/tsc/internal/project/projectcollectionbuilder.go:629-671
func (r *TsConfigResolver) FindTsconfigForFile(filePath string, skipSearchInDirectoryOfFile bool) (configPath string, found bool) {
	configFileName := r.configFileRegistryBuilder.ComputeConfigFileName(r.toFileName(filePath), skipSearchInDirectoryOfFile, nil).AsString()

	if configFileName == "" {
		return "", false
	}

	normalizedPath := r.toPath(filePath)

	// Search through the config and its references
	// This corresponds to findOrCreateDefaultConfiguredProjectWorker
	result := r.findConfigWithReferences(filePath, normalizedPath, configFileName, nil, nil)

	if result.configFileName != "" {
		return result.configFileName, true
	}

	return "", false
}

// Reference: `searchResult`: typescript/tsc/internal/project/projectcollectionbuilder.go:461-465
type configSearchResult struct {
	configFileName string
}

// Reference: `searchNode`: typescript/tsc/internal/project/projectcollectionbuilder.go:467-471
type searchNode struct {
	configFileName string
}

// Reference: `findOrCreateDefaultConfiguredProjectWorker`: typescript/tsc/internal/project/projectcollectionbuilder.go:480-627
func (r *TsConfigResolver) findConfigWithReferences(
	fileName string,
	path tspath.PathKey,
	configFileName string,
	visited *collections.SyncSet[searchNode],
	fallback *configSearchResult,
) configSearchResult {
	var configs collections.SyncMap[tspath.PathKey, *tsoptions.ParsedCommandLine]
	if visited == nil {
		visited = &collections.SyncSet[searchNode]{}
	}

	search := BreadthFirstSearch(
		searchNode{configFileName: configFileName},
		func(node searchNode) []searchNode {
			if config, ok := configs.Load(r.toPath(node.configFileName)); ok && len(config.ProjectReferences()) > 0 {
				references := config.ResolvedProjectReferencePaths()
				return Map(references, func(configFileName tspath.RootedFilePath) searchNode {
					return searchNode{configFileName: configFileName.AsString()}
				})
			}
			return nil
		},
		func(node searchNode) (isResult bool, stop bool) {
			configFilePath := r.toPath(node.configFileName)

			config := r.configFileRegistryBuilder.FindOrAcquireConfigForFile(
				r.toFileName(node.configFileName), configFilePath, path, project.ProjectLoadKindCreate, nil,
			)
			if config == nil {
				return false, false
			}
			configs.Store(configFilePath, config)
			if len(config.FileNames()) == 0 {
				return false, false
			}
			if config.CompilerOptions().Composite == core.TSTrue {
				// For composite projects, we can get an early negative result.
				// !!! what about declaration files in node_modules? wouldn't it be better to
				//     check project inclusion if the project is already loaded?
				if !config.PossiblyMatchesFileName(r.toFileName(fileName)) {
					return false, false
				}
			}

			if config.FilePaths().Has(path) {
				return true, true
			}

			return false, false
		},
		BreadthFirstSearchOptions[searchNode]{
			Visited: visited,
			PreprocessLevel: func(level *BreadthFirstSearchLevel[searchNode]) {
				level.Range(func(node searchNode) bool {
					return true
				})
			},
		},
	)

	tsconfig := ""
	if len(search.Path) > 0 {
		tsconfig = search.Path[0].configFileName
	} else {
		tsconfig = ""
	}

	if search.Stopped {
		return configSearchResult{configFileName: tsconfig}
	}
	if tsconfig != "" {
		fallback = &configSearchResult{configFileName: tsconfig}
	}

	// Look for tsconfig.json files higher up the directory tree and do the same. This handles
	// the common case where a higher-level "solution" tsconfig.json contains all projects in a
	// workspace.
	if config, ok := configs.Load(r.toPath(configFileName)); ok && config.CompilerOptions().DisableSolutionSearching.IsTrue() {
		if fallback != nil {
			return *fallback
		}
	}

	if ancestorConfigName := r.getAncestorConfigFileName(fileName, path, configFileName); ancestorConfigName != "" {
		return r.findConfigWithReferences(
			fileName,
			path,
			ancestorConfigName,
			visited,
			fallback,
		)
	}
	if fallback != nil {
		return *fallback
	}

	return configSearchResult{configFileName: ""}
}

func (r *TsConfigResolver) getAncestorConfigFileName(fileName string, path tspath.PathKey, nearestConfigFileName string) string {
	ancestorConfigName := r.configFileRegistryBuilder.GetAncestorConfigFileName(r.toFileName(fileName), path, r.toFileName(nearestConfigFileName), nil)
	if ancestorConfigName != "" {
		return ancestorConfigName.AsString()
	}
	// Intentionally pass the nearest tsconfig path so the ancestor search
	// resumes from that config's parent directory rather than from the source file.
	return r.configFileRegistryBuilder.ComputeConfigFileName(r.toFileName(nearestConfigFileName), true, nil).AsString()
}

type ResolutionResult struct {
	file   string
	config string
}

const resolutionBatchSize = 16

type resolutionTask struct {
	files      []string
	configName string
	search     bool
}

func (r *TsConfigResolver) work(in chan resolutionTask, out chan<- ResolutionResult, pending *sync.WaitGroup) {
	for task := range in {
		if task.search {
			for _, file := range task.files {
				filePath := r.toPath(file)
				result := r.findConfigWithReferences(file, filePath, task.configName, nil, nil)
				out <- ResolutionResult{file: file, config: result.configFileName}
			}
		} else {
			r.resolveDirectory(task.files, in, out, pending)
		}
		pending.Done()
	}
}

func (r *TsConfigResolver) resolveDirectory(files []string, in chan<- resolutionTask, out chan<- ResolutionResult, pending *sync.WaitGroup) {
	// ComputeConfigFileName only inspects the containing directory and its
	// ancestors. Files in one directory therefore share this lookup.
	configName := r.configFileRegistryBuilder.ComputeConfigFileName(r.toFileName(files[0]), false, nil).AsString()
	var includedFiles interface{ Has(tspath.PathKey) bool }
	if configName != "" {
		configPath := r.toPath(configName)
		config := r.configFileRegistryBuilder.FindOrAcquireConfigForFile(
			r.toFileName(configName), configPath, r.toPath(files[0]), project.ProjectLoadKindCreate, nil,
		)
		if config != nil {
			// A file included by the nearest config cannot be claimed by a
			// reference or ancestor. Only misses need the full search.
			includedFiles = config.FilePaths()
		}
	}
	var misses []string
	for _, file := range files {
		if configName == "" {
			out <- ResolutionResult{file: file}
			continue
		}

		if includedFiles != nil && includedFiles.Has(r.toPath(file)) {
			out <- ResolutionResult{file: file, config: configName}
			continue
		}

		misses = append(misses, file)
	}
	// Keep expensive reference and ancestor searches parallel even when all
	// files came from the same directory. This directory task remains pending
	// until its batches are queued, so the queue cannot close during Add.
	pending.Add((len(misses) + resolutionBatchSize - 1) / resolutionBatchSize)
	for len(misses) > 0 {
		batchSize := min(len(misses), resolutionBatchSize)
		in <- resolutionTask{files: misses[:batchSize], configName: configName, search: true}
		misses = misses[batchSize:]
	}
}

func (r *TsConfigResolver) FindTsConfigParallel(fileNames []string) map[string]string {
	if len(fileNames) == 0 {
		return map[string]string{}
	}

	filesByDirectory := make(map[string][]string)
	for _, file := range fileNames {
		directory := tspath.GetDirectoryPath(file)
		filesByDirectory[directory] = append(filesByDirectory[directory], file)
	}

	// The buffer holds all directory tasks and any batches they can enqueue,
	// so workers never block each other while dispatching misses.
	in := make(chan resolutionTask, len(filesByDirectory)+(len(fileNames)+resolutionBatchSize-1)/resolutionBatchSize)
	out := make(chan ResolutionResult, len(fileNames))

	numWorker := runtime.GOMAXPROCS(0)

	var pending sync.WaitGroup
	pending.Add(len(filesByDirectory))
	for range numWorker {
		go r.work(in, out, &pending)
	}

	for _, files := range filesByDirectory {
		in <- resolutionTask{files: files}
	}

	go func() {
		pending.Wait()
		close(in)
		close(out)
	}()

	res := make(map[string]string, len(fileNames))
	for result := range out {
		res[result.file] = result.config
	}

	return res
}

// Reference: `toPath`: typescript/tsc/internal/project/projectcollectionbuilder.go:687-689
func (b *TsConfigResolver) toPath(fileName string) tspath.PathKey {
	return b.fs.CaseSensitivity().PathKey(b.toFileName(fileName).AsPath())
}

func (b *TsConfigResolver) toFileName(fileName string) tspath.RootedFilePath {
	return tspath.ToRootedFilePath(fileName, b.currentDirectoryPath)
}

func (r *TsConfigResolver) FS() vfs.FS {
	return r.fs
}

func (r *TsConfigResolver) GetCurrentDirectory() string {
	return r.currentDirectory
}
