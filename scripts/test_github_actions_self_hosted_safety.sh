#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
build_workflow="$repo_root/.github/workflows/build.yml"
actionlint_config="$repo_root/actionlint.yaml"

ruby - "$repo_root" <<'RUBY'
require "yaml"

repo_root = ARGV.fetch(0)
workflow_dir = File.join(repo_root, ".github", "workflows")

def runs_on_values(value)
  case value
  when String
    [value]
  when Array
    value.flat_map { |entry| runs_on_values(entry) }
  else
    []
  end
end

def dynamic_self_hosted_capable?(runs_on)
  runs_on_values(runs_on).any? do |value|
    value.include?("inputs.runner") || value.match?(/vars\.AICO_RUNNER_(?:PRIMARY|BACKUP)\b/)
  end
end

def dedicated_hosted_label?(value)
  value.start_with?("aico-hosted-", "aiden-hosted-")
end

def self_hosted_capable?(runs_on)
  values = runs_on_values(runs_on)
  values.include?("self-hosted") ||
    values.any? { |value| dedicated_hosted_label?(value) } ||
    dynamic_self_hosted_capable?(runs_on)
end

def repo_branch_guarded?(condition, dynamic_runner)
  normalized = condition.gsub(/\s+/, " ")
  blocks_pr_events =
    normalized.include?("github.event_name != 'pull_request'") &&
    normalized.include?("github.event_name != 'pull_request_target'")
  requires_repo_branch =
    normalized.include?("startsWith(github.ref, 'refs/heads/')") ||
    normalized.include?('startsWith(github.ref, "refs/heads/")')
  # A dynamic build may exempt GitHub-hosted inputs from the branch guard, but
  # that exemption must exclude both the canonical aliases and legacy labels.
  runner_guard = !dynamic_runner || !normalized.include?("||") || (
    normalized.include?("inputs.runner != 'self-hosted'") &&
    normalized.include?("!startsWith(inputs.runner, 'aico-hosted-')") &&
    normalized.include?("!startsWith(inputs.runner, 'aiden-hosted-')")
  )

  blocks_pr_events && requires_repo_branch && runner_guard
end

failures = []

branch_guard = "startsWith(github.ref, 'refs/heads/') && github.event_name != 'pull_request' && github.event_name != 'pull_request_target'"
%w[self-hosted aico-hosted-01 aico-hosted-02 aiden-hosted-01 aiden-hosted-02].each do |label|
  abort "self-hosted safety check failed to recognize #{label}" unless self_hosted_capable?(label)
end
["${{ inputs.runner }}", "${{ vars.AICO_RUNNER_PRIMARY || 'aiden-hosted-01' }}", "${{ vars.AICO_RUNNER_BACKUP || 'aiden-hosted-02' }}"].each do |expression|
  abort "self-hosted safety check failed to recognize #{expression}" unless self_hosted_capable?(expression)
end
abort "hosted Ubuntu runner incorrectly classified as self-hosted" if self_hosted_capable?("ubuntu-24.04")
abort "unconditional repository branch guard was rejected" unless repo_branch_guarded?(branch_guard, true)
abort "missing repository branch guard was accepted" if repo_branch_guarded?("", false)
%w[aico aiden].each do |brand|
  incomplete_guard = "(inputs.runner != 'self-hosted' && !startsWith(inputs.runner, '#{brand}-hosted-')) || (#{branch_guard})"
  abort "runner exemption omitted a dedicated label prefix" if repo_branch_guarded?(incomplete_guard, true)
end

Dir.glob(File.join(workflow_dir, "*.{yml,yaml}")).sort.each do |file|
  workflow = YAML.load_file(file)
  jobs = workflow.fetch("jobs", {})
  jobs.each do |job_name, job|
    next unless job.is_a?(Hash)
    runs_on = job["runs-on"]
    next unless self_hosted_capable?(runs_on)

    dynamic_runner = dynamic_self_hosted_capable?(runs_on)
    condition = job["if"].to_s
    next if repo_branch_guarded?(condition, dynamic_runner)

    relative_file = file.delete_prefix("#{repo_root}/")
    failures << "#{relative_file}:#{job_name} can run on self-hosted without a job-level repo-branch guard"
  end
end

if failures.any?
  warn "Unsafe self-hosted GitHub Actions configuration:"
  failures.each { |failure| warn " - #{failure}" }
  exit 1
end

puts "GitHub Actions self-hosted repo-branch safety check passed."
RUBY

if [[ ! -f "$actionlint_config" ]]; then
  echo "actionlint config must declare dedicated Aico hosted runner labels" >&2
  exit 1
fi

ruby - "$actionlint_config" <<'RUBY'
require "yaml"

config_path = ARGV.fetch(0)
config = YAML.load_file(config_path) || {}
labels = config.dig("self-hosted-runner", "labels")
unless labels.is_a?(Array)
  warn "actionlint config must define self-hosted-runner.labels"
  exit 1
end

missing_labels = %w[aico-hosted-01 aico-hosted-02 aiden-hosted-01 aiden-hosted-02] - labels
if missing_labels.any?
  warn "actionlint config is missing canonical or legacy dedicated runner labels: #{missing_labels.join(", ")}"
  exit 1
end
RUBY

ruby - "$build_workflow" "$repo_root/.github/workflows/benchmark.yml" <<'RUBY'
require "yaml"

build_path, benchmark_path = ARGV
build = YAML.load_file(build_path).fetch("jobs").fetch("build")
runner_mapping = build.fetch("runs-on")
{
  "aico-hosted-01" => ["AICO_RUNNER_PRIMARY", "aiden-hosted-01"],
  "aico-hosted-02" => ["AICO_RUNNER_BACKUP", "aiden-hosted-02"],
}.each do |canonical, (variable, registered)|
  expected = "inputs.runner == '#{canonical}' && (vars.#{variable} || '#{registered}')"
  abort "build runner mapping must retain the registered #{registered} default" unless runner_mapping.include?(expected)
end
abort "build runner mapping must preserve explicit hosted runner inputs" unless runner_mapping.end_with?("|| inputs.runner }}")
benchmark_runner = YAML.load_file(benchmark_path).fetch("jobs").fetch("run").fetch("runs-on")
unless benchmark_runner == "${{ vars.AICO_RUNNER_PRIMARY || 'aiden-hosted-02' }}"
  abort "benchmark must use the configurable primary runner with its existing registration as the default"
end

%w[aico aiden].each do |brand|
  prefix = "startsWith(inputs.runner, '#{brand}-hosted-')"
  ["Reclaim workspace ownership", "Remove legacy pico-sdk Git metadata", "Install hosted runner prerequisites"].each do |name|
    condition = build.fetch("steps").find { |step| step["name"] == name }.fetch("if")
    abort "#{name} must recognize #{brand} runner labels" unless condition.include?(prefix)
  end
end
RUBY

ruby - "$build_workflow" <<'RUBY'
require "fileutils"
require "tmpdir"
require "yaml"

workflow_path = ARGV.fetch(0)
steps = YAML.load_file(workflow_path).fetch("jobs").fetch("build").fetch("steps")
ownership_index = steps.index { |step| step["name"] == "Reclaim workspace ownership" }
cleanup_index = steps.index { |step| step["name"] == "Remove legacy pico-sdk Git metadata" }
checkout_index = steps.index { |step| step["uses"].to_s.start_with?("actions/checkout@") }
unless ownership_index && cleanup_index && checkout_index &&
       ownership_index < cleanup_index && cleanup_index < checkout_index
  abort "self-hosted build workflow must reclaim ownership and remove legacy SDK Git metadata before checkout"
end

cleanup = steps.fetch(cleanup_index).fetch("run")
%w[gitfile gitdir orphaned_metadata tracked absent].each do |scenario|
  Dir.mktmpdir("aico-sdk-checkout-") do |workspace|
    sdk_dir = File.join(workspace, "pico-sdk")
    sdk_git_dir = File.join(workspace, ".git", "modules", "pico-sdk")
    mobilegym_dir = File.join(workspace, "benchmark", "mobilegym", "vendor", "mobilegym")
    FileUtils.mkdir_p(mobilegym_dir)
    File.write(File.join(mobilegym_dir, ".git"), "gitdir: preserved\n")
    unless scenario == "absent"
      FileUtils.mkdir_p(sdk_dir)
      File.write(File.join(sdk_dir, "source.c"), "tracked SDK source\n")
    end
    if scenario == "gitfile"
      # Include a broken checkout; cleanup must not depend on a readable HEAD.
      File.write(File.join(sdk_dir, ".git"), "gitdir: missing\n")
      FileUtils.mkdir_p(sdk_git_dir)
    elsif scenario == "gitdir"
      FileUtils.mkdir_p(File.join(sdk_dir, ".git"))
    elsif scenario == "orphaned_metadata"
      FileUtils.mkdir_p(sdk_git_dir)
    end

    unless system({ "GITHUB_WORKSPACE" => workspace }, "bash", "-c", cleanup)
      abort "legacy SDK cleanup failed for #{scenario}"
    end
    if scenario == "tracked"
      abort "legacy SDK cleanup removed ordinary SDK sources" unless File.file?(File.join(sdk_dir, "source.c"))
    elsif File.exist?(sdk_dir) || File.exist?(sdk_git_dir)
      abort "legacy SDK cleanup left a stale checkout for #{scenario}"
    end
    abort "legacy SDK cleanup removed MobileGym metadata" unless File.file?(File.join(mobilegym_dir, ".git"))
  end
end

Dir.glob(File.join(File.dirname(workflow_path), "*.{yml,yaml}")).each do |file|
  if File.read(file).match?(/git\s+submodule[^\n]*\bpico-sdk\b/)
    abort "#{file}: workflows must use the tracked SDK sources instead of fetching a submodule"
  end
end
puts "Vendored SDK checkout migration check passed."
RUBY

goproxy_line="$(grep -n 'GOPROXY=https://goproxy.cn' "$build_workflow" | sed 's/:.*//' | head -n 1 || true)"
setup_go_line="$(grep -n 'actions/setup-go@' "$build_workflow" | sed 's/:.*//' | head -n 1 || true)"
run_build_line="$(grep -n 'Run Debian firmware build' "$build_workflow" | sed 's/:.*//' | head -n 1 || true)"
if [[ -z "$goproxy_line" || -z "$setup_go_line" || -z "$run_build_line" || \
      "$goproxy_line" -ge "$setup_go_line" || "$goproxy_line" -ge "$run_build_line" ]]; then
  echo "self-hosted build workflow must configure GOPROXY before Go setup and image build" >&2
  exit 1
fi
