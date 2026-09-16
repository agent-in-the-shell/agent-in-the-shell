package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/factory"
	"gopkg.in/yaml.v3"
)

type discoveredChoice struct {
	Source factory.DiscoverySource
	Model  string
}

func doConfigureModels(args []string, in io.Reader, out, errOut io.Writer, logger *slog.Logger) error {
	fs := flag.NewFlagSet("configure-models", flag.ContinueOnError)
	fs.SetOutput(errOut)
	configPath := fs.String("config", "", "path to YAML config")
	timeout := fs.Duration("timeout", 10*time.Second, "timeout per upstream model-list request")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *timeout <= 0 {
		return errors.New("--timeout must be positive")
	}
	path := *configPath
	if path == "" {
		path = agentmodel.DefaultConfigPath()
	}
	original, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	cfg, err := agentmodel.LoadConfig(path)
	if err != nil {
		return err
	}

	sources, unsupported, err := factory.DiscoverSources(cfg, logger)
	if err != nil {
		return fmt.Errorf("build discovery sources: %w", err)
	}
	for _, msg := range unsupported {
		fmt.Fprintf(errOut, "Skipping %s\n", msg)
	}

	existingNames := make(map[string]bool, len(cfg.ModelList))
	existingUpstream := make(map[string]bool)
	for _, model := range cfg.ModelList {
		existingNames[model.ModelName] = true
		for _, deployment := range model.Deployments {
			existingUpstream[deployment.Provider+"\x00"+deployment.Model] = true
		}
	}

	var choices []discoveredChoice
	succeeded := 0
	for _, source := range sources {
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		models, listErr := source.Lister.ListModels(ctx)
		cancel()
		if listErr != nil {
			fmt.Fprintf(errOut, "Discovery failed for %s: %v\n", source.Name, listErr)
			continue
		}
		succeeded++
		sort.Strings(models)
		seen := make(map[string]bool)
		for _, model := range models {
			model = strings.TrimSpace(model)
			if model == "" || seen[model] || existingNames[model] || existingUpstream[source.Template.Provider+"\x00"+model] {
				continue
			}
			seen[model] = true
			choices = append(choices, discoveredChoice{Source: source, Model: model})
		}
	}
	if succeeded == 0 {
		return errors.New("model discovery failed for every configured source")
	}
	if len(choices) == 0 {
		fmt.Fprintln(out, "No new upstream models found.")
		return nil
	}

	fmt.Fprintln(out, "New upstream models:")
	lastSource := ""
	for i, choice := range choices {
		if choice.Source.Name != lastSource {
			fmt.Fprintf(out, "\n%s\n", choice.Source.Name)
			lastSource = choice.Source.Name
		}
		fmt.Fprintf(out, "  %2d) %s\n", i+1, choice.Model)
	}
	fmt.Fprintln(out)
	fmt.Fprint(out, "Select models (for example 1,3-5; blank cancels): ")
	reader := bufio.NewReader(in)
	line, readErr := reader.ReadString('\n')
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return readErr
	}
	line = strings.TrimSpace(line)
	if line == "" {
		fmt.Fprintln(out, "Cancelled.")
		return nil
	}
	selected, err := parseNumberSelection(line, len(choices))
	if err != nil {
		return err
	}

	byModel := make(map[string][]agentmodel.DeploymentConfig)
	var order []string
	for _, index := range selected {
		choice := choices[index]
		deployment := choice.Source.Template
		deployment.Model = choice.Model
		if deployment.Provider == "azure" {
			deployment.DeploymentName = choice.Model
		}
		if _, ok := byModel[choice.Model]; !ok {
			order = append(order, choice.Model)
		}
		byModel[choice.Model] = append(byModel[choice.Model], deployment)
	}
	additions := make([]agentmodel.ModelEntry, 0, len(order))
	fmt.Fprintln(out, "\nPlanned additions:")
	for _, model := range order {
		if existingNames[model] {
			continue
		}
		entry := agentmodel.ModelEntry{ModelName: model, Deployments: byModel[model]}
		additions = append(additions, entry)
		fmt.Fprintf(out, "  %s\n", model)
		for _, deployment := range entry.Deployments {
			fmt.Fprintf(out, "    - %s (%s)\n", deployment.Provider, deployment.AuthMode)
		}
	}
	if len(additions) == 0 {
		fmt.Fprintln(out, "Nothing to add.")
		return nil
	}
	fmt.Fprint(out, "Type yes to update the config: ")
	confirmation, confirmErr := reader.ReadString('\n')
	if confirmErr != nil && !errors.Is(confirmErr, io.EOF) {
		return confirmErr
	}
	if strings.TrimSpace(confirmation) != "yes" {
		fmt.Fprintln(out, "Cancelled.")
		return nil
	}

	candidate := *cfg
	candidate.ModelList = append(append([]agentmodel.ModelEntry(nil), cfg.ModelList...), additions...)
	if err := candidate.Validate(); err != nil {
		return fmt.Errorf("updated config is invalid: %w", err)
	}
	updated, err := appendModelEntries(original, additions)
	if err != nil {
		return err
	}
	if err := atomicReplaceConfig(path, original, updated); err != nil {
		return err
	}
	fmt.Fprintf(out, "Updated %s. Restart agent-model to apply the new models.\n", path)
	return nil
}

func parseNumberSelection(input string, count int) ([]int, error) {
	selected := make(map[int]bool)
	for _, part := range strings.Split(input, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, errors.New("invalid empty selection")
		}
		bounds := strings.Split(part, "-")
		if len(bounds) > 2 {
			return nil, fmt.Errorf("invalid selection %q", part)
		}
		first, err := strconv.Atoi(strings.TrimSpace(bounds[0]))
		if err != nil {
			return nil, fmt.Errorf("invalid selection %q", part)
		}
		last := first
		if len(bounds) == 2 {
			last, err = strconv.Atoi(strings.TrimSpace(bounds[1]))
			if err != nil || last < first {
				return nil, fmt.Errorf("invalid range %q", part)
			}
		}
		if first < 1 || last > count {
			return nil, fmt.Errorf("selection %q is outside 1-%d", part, count)
		}
		for n := first; n <= last; n++ {
			selected[n-1] = true
		}
	}
	out := make([]int, 0, len(selected))
	for n := range selected {
		out = append(out, n)
	}
	sort.Ints(out)
	return out, nil
}

func appendModelEntries(original []byte, additions []agentmodel.ModelEntry) ([]byte, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(original, &document); err != nil {
		return nil, fmt.Errorf("parse config for editing: %w", err)
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("config root must be a YAML mapping")
	}
	root := document.Content[0]
	var list *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "model_list" {
			list = root.Content[i+1]
			break
		}
	}
	if list == nil || list.Kind != yaml.SequenceNode {
		return nil, errors.New("config model_list must be a YAML sequence")
	}
	for _, addition := range additions {
		var node yaml.Node
		if err := node.Encode(addition); err != nil {
			return nil, fmt.Errorf("encode model entry: %w", err)
		}
		list.Content = append(list.Content, &node)
	}
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(&document); err != nil {
		return nil, fmt.Errorf("encode updated config: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func atomicReplaceConfig(path string, original, updated []byte) error {
	current, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("re-read config: %w", err)
	}
	if !bytes.Equal(current, original) {
		return errors.New("config changed while models were being selected; refusing to overwrite it")
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat config: %w", err)
	}
	ownership, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("config ownership is unavailable on this platform")
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".agentmodel-config-*")
	if err != nil {
		return fmt.Errorf("create temporary config: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	// Rename publishes the temporary inode, including its ownership. Preserve
	// the original uid/gid so editing through a group-writable config directory
	// cannot silently make the gateway's config unreadable after restart.
	if err := tmp.Chown(int(ownership.Uid), int(ownership.Gid)); err != nil {
		tmp.Close()
		return fmt.Errorf("preserve config ownership: %w", err)
	}
	if _, err := tmp.Write(updated); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
