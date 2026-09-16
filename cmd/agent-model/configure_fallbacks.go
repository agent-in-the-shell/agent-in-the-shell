package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"gopkg.in/yaml.v3"
)

func doConfigureFallbacks(args []string, in io.Reader, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("configure-fallbacks", flag.ContinueOnError)
	fs.SetOutput(errOut)
	configPath := fs.String("config", "", "path to YAML config")
	if err := fs.Parse(args); err != nil {
		return err
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
	reader := bufio.NewReader(in)

	fmt.Fprintln(out, "Models:")
	for i, model := range cfg.ModelList {
		current := fallbackTargets(cfg.Fallbacks, model.ModelName)
		suffix := ""
		if len(current) > 0 {
			suffix = "  → " + strings.Join(current, " → ")
		}
		fmt.Fprintf(out, "  %2d) %s%s\n", i+1, model.ModelName, suffix)
	}
	fmt.Fprint(out, "Select the primary model to edit (blank cancels): ")
	line, err := readInteractiveLine(reader)
	if err != nil {
		return err
	}
	if line == "" {
		fmt.Fprintln(out, "Cancelled.")
		return nil
	}
	primaryIndex, err := parseSingleNumber(line, len(cfg.ModelList))
	if err != nil {
		return err
	}
	primary := cfg.ModelList[primaryIndex].ModelName

	var candidates []string
	fmt.Fprintf(out, "\nFallback order for %s:\n", primary)
	for _, model := range cfg.ModelList {
		if model.ModelName == primary {
			continue
		}
		candidates = append(candidates, model.ModelName)
		fmt.Fprintf(out, "  %2d) %s\n", len(candidates), model.ModelName)
	}
	fmt.Fprint(out, "Enter ordered choices (for example 3,1), 'none' to clear, or blank to cancel: ")
	line, err = readInteractiveLine(reader)
	if err != nil {
		return err
	}
	if line == "" {
		fmt.Fprintln(out, "Cancelled.")
		return nil
	}
	var targets []string
	if line != "none" {
		selected, err := parseOrderedSelection(line, len(candidates))
		if err != nil {
			return err
		}
		for _, index := range selected {
			targets = append(targets, candidates[index])
		}
	}

	rules := replaceFallbackRule(cfg.Fallbacks, primary, targets)
	candidate := *cfg
	candidate.Fallbacks = rules
	if err := candidate.Validate(); err != nil {
		return fmt.Errorf("updated config is invalid: %w", err)
	}
	if len(targets) == 0 {
		fmt.Fprintf(out, "\nPlanned change: remove fallback for %s\n", primary)
	} else {
		fmt.Fprintf(out, "\nPlanned change: %s → %s\n", primary, strings.Join(targets, " → "))
	}
	fmt.Fprint(out, "Type yes to update the config: ")
	confirmation, err := readInteractiveLine(reader)
	if err != nil {
		return err
	}
	if confirmation != "yes" {
		fmt.Fprintln(out, "Cancelled.")
		return nil
	}
	updated, err := replaceFallbacksNode(original, rules)
	if err != nil {
		return err
	}
	if err := atomicReplaceConfig(path, original, updated); err != nil {
		return err
	}
	fmt.Fprintf(out, "Updated %s. Restart agent-model to apply the fallback order.\n", path)
	return nil
}

func readInteractiveLine(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func parseSingleNumber(input string, count int) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(input))
	if err != nil || n < 1 || n > count {
		return 0, fmt.Errorf("selection %q is outside 1-%d", input, count)
	}
	return n - 1, nil
}

func parseOrderedSelection(input string, count int) ([]int, error) {
	var ordered []int
	seen := make(map[int]bool)
	for _, part := range strings.Split(input, ",") {
		part = strings.TrimSpace(part)
		if part == "" || strings.Contains(part, "-") {
			return nil, fmt.Errorf("invalid ordered selection %q", part)
		}
		index, err := parseSingleNumber(part, count)
		if err != nil {
			return nil, err
		}
		if seen[index] {
			return nil, fmt.Errorf("duplicate selection %d", index+1)
		}
		seen[index] = true
		ordered = append(ordered, index)
	}
	return ordered, nil
}

func fallbackTargets(rules []agentmodel.FallbackRule, model string) []string {
	for _, rule := range rules {
		if rule.ModelName == model {
			return rule.FallbackTo
		}
	}
	return nil
}

func replaceFallbackRule(rules []agentmodel.FallbackRule, model string, targets []string) []agentmodel.FallbackRule {
	out := make([]agentmodel.FallbackRule, 0, len(rules)+1)
	replaced := false
	for _, rule := range rules {
		if rule.ModelName != model {
			out = append(out, rule)
			continue
		}
		if !replaced && len(targets) > 0 {
			out = append(out, agentmodel.FallbackRule{ModelName: model, FallbackTo: targets})
		}
		replaced = true
	}
	if !replaced && len(targets) > 0 {
		out = append(out, agentmodel.FallbackRule{ModelName: model, FallbackTo: targets})
	}
	return out
}

func replaceFallbacksNode(original []byte, rules []agentmodel.FallbackRule) ([]byte, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(original, &document); err != nil {
		return nil, fmt.Errorf("parse config for editing: %w", err)
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("config root must be a YAML mapping")
	}
	root := document.Content[0]
	var keyIndex = -1
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "fallbacks" {
			keyIndex = i
			break
		}
	}
	if len(rules) == 0 {
		if keyIndex >= 0 {
			root.Content = append(root.Content[:keyIndex], root.Content[keyIndex+2:]...)
		}
	} else {
		var node yaml.Node
		if err := node.Encode(rules); err != nil {
			return nil, fmt.Errorf("encode fallbacks: %w", err)
		}
		if keyIndex >= 0 {
			root.Content[keyIndex+1] = &node
		} else {
			root.Content = append(root.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "fallbacks"}, &node)
		}
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
