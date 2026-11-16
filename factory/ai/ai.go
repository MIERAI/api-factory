// Package ai: 调用 AI 的唯一入口。模型(怎么调用)和角色(用哪个模型、允许哪些)都在 factory.yaml 里,
// 脚本和其他环节不写死任何模型、命令或环境变量。
package ai

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Models map[string]Model `yaml:"models"`
	Roles  map[string]Role  `yaml:"roles"`
}

type Model struct {
	Command []string          `yaml:"command"`
	Env     map[string]string `yaml:"env"`
}

type Role struct {
	Model      string   `yaml:"model"`
	Allowed    []string `yaml:"allowed"`
	Args       []string `yaml:"args"`
	TimeoutSec int      `yaml:"timeout_sec"`
}

func Load() (*Config, error) {
	b, err := os.ReadFile("factory.yaml")
	if err != nil {
		return nil, fmt.Errorf("factory.yaml: %w", err)
	}
	var f struct {
		AI Config `yaml:"ai"`
	}
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("factory.yaml: %w", err)
	}
	return &f.AI, nil
}

// Resolve: 角色 + (可选的)模型 → 要执行的命令和环境变量。违反角色的策略时报错。
func (c *Config) Resolve(role, model string, extra []string) ([]string, []string, time.Duration, error) {
	r, ok := c.Roles[role]
	if !ok {
		return nil, nil, 0, fmt.Errorf("ai: unknown role %q", role)
	}
	if model == "" {
		model = r.Model
	}
	if len(r.Allowed) > 0 && !slices.Contains(r.Allowed, model) {
		return nil, nil, 0, fmt.Errorf("ai: model %q is not allowed for role %q (allowed: %v; see factory.yaml)", model, role, r.Allowed)
	}
	m, ok := c.Models[model]
	if !ok || len(m.Command) == 0 {
		return nil, nil, 0, fmt.Errorf("ai: unknown model %q", model)
	}
	argv := append(append(append([]string{}, m.Command...), r.Args...), extra...)
	env := os.Environ()
	for k, v := range m.Env {
		env = append(env, k+"="+v)
	}
	timeout := time.Duration(r.TimeoutSec) * time.Second
	if timeout == 0 {
		timeout = 30 * time.Minute
	}
	return argv, env, timeout, nil
}

// Run: prompt 从 stdin 进,结果写到 stdout。
func Run(role, model string, extra []string, stdin io.Reader, stdout, stderr io.Writer) error {
	c, err := Load()
	if err != nil {
		return err
	}
	argv, env, timeout, err := c.Resolve(role, model, extra)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = env, stdin, stdout, stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("ai: %s timed out after %s", role, timeout)
		}
		return err
	}
	return nil
}
