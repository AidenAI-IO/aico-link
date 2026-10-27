#!/bin/sh
set -eu
ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)

ruby - "$ROOT_DIR" <<'RUBY'
require 'yaml'
root = ARGV.fetch(0)
load_workflow = ->(name) { YAML.load_file(File.join(root, '.github/workflows', name)) }
release = load_workflow.call('release.yml')
events = release['on'] || release[true]
raise 'channel releases must have only manual/reusable entrypoints' unless events.keys.sort == %w[workflow_call workflow_dispatch]
inputs = events['workflow_dispatch']['inputs']
call = events['workflow_call']
raise 'reusable release inputs must match manual inputs' unless call['inputs'].keys.sort == inputs.keys.sort
inputs.each do |name, input|
  expected_type = input['type'] == 'choice' ? 'string' : input['type']
  raise "reusable #{name} input type differs" unless call['inputs'][name]['type'] == expected_type
  raise "reusable #{name} default differs" unless call['inputs'][name]['default'] == input['default']
end
raise 'reusable channel must be explicit' unless call['inputs']['channel']['required']
%w[OTA_ED25519_PRIVATE_KEY AGENT_CONFIG_TOML].each do |secret|
  raise "#{secret} must be optional for preview/business builds" unless call['secrets'][secret]['required'] == false
end
raise 'three channel choices required' unless inputs['channel']['options'] == %w[dev staging prod]
raise 'preview must not publish by default' unless inputs['plan_only']['default'] && !inputs['publish']['default']
raise 'all channels must share one lock' unless release['concurrency'] == {
  'group' => 'aico-channel-release', 'cancel-in-progress' => false
}
jobs = release['jobs']
raise 'plan must be read-only' unless release['permissions']['contents'] == 'read'
raise 'publication needs channel environment' unless jobs['publish']['environment'] == '${{ inputs.channel }}'
raise 'only publication needs write' unless jobs['publish']['permissions']['contents'] == 'write'
raise 'business job must depend on classification' unless jobs['business']['if'].include?("kind == 'business'")
raise 'OTA job must depend on classification' unless jobs['ota']['if'].include?("kind == 'ota'")
raise 'OTA must reuse the system build' unless jobs['ota']['uses'] == './.github/workflows/build.yml'
raise 'APT indexing must follow successful publication' unless jobs['apt']['needs'] == 'publish' && jobs['apt']['if'].include?("needs.publish.result == 'success'")
raise 'APT must use the shared index workflow' unless jobs['apt']['uses'] == './.github/workflows/apt-repository.yml'
%w[pages id-token].each do |permission|
  raise "APT caller must grant #{permission}" unless jobs['apt']['permissions'][permission] == 'write'
end
apt = load_workflow.call('apt-repository.yml')
apt_events = apt['on'] || apt[true]
raise 'APT indexes must update only on publication or manual refresh' unless apt_events.keys.sort == %w[workflow_call workflow_dispatch]
raise 'APT deploys must serialize' unless apt['concurrency'] == {'group' => 'aico-apt-pages', 'cancel-in-progress' => false}
raise 'APT deployment must depend on verified output' unless apt['jobs']['deploy']['needs'] == 'build'

backup = load_workflow.call('build-backup.yml')
backup_events = backup['on'] || backup[true]
raise 'backup releases must be manual only' unless backup_events.keys == ['workflow_dispatch']
backup_inputs = backup_events['workflow_dispatch']['inputs']
raise 'backup must expose three channels' unless backup_inputs['channel']['options'] == inputs['channel']['options']
raise 'backup must default to verified publication' unless backup_inputs['plan_only']['default'] == false && backup_inputs['publish']['default'] == true && backup_inputs['dry_run']['default'] == false && backup_inputs['apt_only']['default'] == false
backup_jobs = backup['jobs']
raise 'backup must reuse release and APT publishers' unless backup_jobs.keys.sort == %w[apt preflight release]
raise 'APT-only must not enter release flow' unless backup_jobs['release']['if'].include?('!inputs.apt_only')
raise 'APT-only must reuse index workflow' unless backup_jobs['apt']['uses'] == './.github/workflows/apt-repository.yml'
raise 'preflight must override APT-only' unless backup_jobs['apt']['if'].include?('!inputs.dry_run')
backup_release = backup_jobs['release']
raise 'backup must reuse channel decisions and publication' unless backup_release['uses'] == './.github/workflows/release.yml'
raise 'backup must use runner 02' unless backup_release['with']['runner'] == 'aico-hosted-02'
%w[channel source_ref version force_ota plan_only publish].each do |name|
  raise "backup does not forward #{name}" unless backup_release['with'][name] == "${{ inputs.#{name} }}"
end
raise 'backup must pass publication permission to callee' unless backup_release['permissions']['contents'] == 'write'
%w[pages id-token].each do |permission|
  raise "backup must pass #{permission} permission to APT deployment" unless backup_release['permissions'][permission] == 'write'
end
raise 'backup must forward secrets' unless backup_release['secrets'] == 'inherit'
raise 'preflight must not enter release flow' unless backup_release['if'].include?('!inputs.dry_run')
preflight = backup_jobs['preflight']
raise 'backup preflight must stay artifact-only' unless preflight['uses'] == './.github/workflows/build.yml' && preflight['with']['dry_run'] == true
raise 'preflight must use backup runner and requested source' unless preflight['with']['runner'] == 'aico-hosted-02' && preflight['with']['source_ref'] == '${{ inputs.source_ref }}'
raise 'preflight must be explicitly enabled' unless preflight['if'].include?('inputs.dry_run &&')
raise 'caller must not acquire the callee release lock' if backup.dig('concurrency', 'group') == release.dig('concurrency', 'group')

primary = load_workflow.call('build-scheduled.yml')
primary_events = primary['on'] || primary[true]
raise 'primary must support hourly and manual runs only' unless primary_events.keys.sort == %w[schedule workflow_dispatch]
raise 'primary must check hourly at minute 17' unless primary_events['schedule'] == [{'cron' => '17 * * * *'}]
primary_inputs = primary_events['workflow_dispatch']['inputs']
raise 'primary must expose the backup inputs' unless primary_inputs.keys.sort == backup_inputs.keys.sort
primary_inputs.each do |name, input|
  %w[type default options required].each do |key|
    raise "primary #{name} #{key} differs from backup" unless input[key] == backup_inputs[name][key]
  end
end
raise 'hourly defaults must target main/dev' unless primary_inputs['channel']['default'] == 'dev' && primary_inputs['source_ref']['default'] == 'main'
raise 'primary must not force OTA by default' unless primary_inputs['force_ota']['default'] == false
raise 'primary must serialize without cancelling active builds' unless primary['concurrency'] == {'group' => 'primary-build', 'cancel-in-progress' => false}
raise 'primary must not acquire the callee release lock' if primary.dig('concurrency', 'group') == release.dig('concurrency', 'group')
primary_jobs = primary['jobs']
raise 'primary must reuse the shared planner and publishers' unless primary_jobs.keys.sort == %w[apt preflight release]
primary_release = primary_jobs['release']
%w[if uses permissions secrets].each do |key|
  raise "primary release #{key} must match backup" unless primary_release[key] == backup_release[key]
end
raise 'primary must supply scheduled defaults and preserve manual overrides' unless primary_release['with'] == {
  'runner' => 'aico-hosted-01',
  'channel' => "${{ inputs.channel || 'dev' }}",
  'source_ref' => "${{ inputs.source_ref || 'main' }}",
  'version' => '${{ inputs.version }}',
  'force_ota' => '${{ inputs.force_ota || false }}',
  'plan_only' => '${{ inputs.plan_only || false }}',
  'publish' => "${{ github.event_name == 'schedule' || inputs.publish }}"
}
expected_preflight = preflight.merge('with' => preflight['with'].merge('runner' => 'aico-hosted-01', 'source_ref' => "${{ inputs.source_ref || 'main' }}"))
raise 'primary preflight must preserve dry-run isolation' unless primary_jobs['preflight'] == expected_preflight
raise 'primary APT-only must preserve dry-run precedence' unless primary_jobs['apt'] == backup_jobs['apt']
%w[business ota].each do |kind|
  raise "#{kind} must skip no-change plans and previews" unless jobs[kind]['if'] == "${{ !inputs.plan_only && needs.plan.outputs.kind == '#{kind}' }}"
end
raise 'publication must require verified assets and explicit enablement' unless jobs['publish']['if'] == "${{ !cancelled() && inputs.publish && !inputs.plan_only && needs.plan.result == 'success' && (needs.business.result == 'success' || needs.ota.result == 'success') }}"

%w[build.yml build-scheduled.yml build-backup.yml build-fallback.yml debian-package.yml].each do |name|
  workflow = load_workflow.call(name)
  triggers = workflow['on'] || workflow[true]
  raise "#{name} must not schedule publication" if name != 'build-scheduled.yml' && triggers.key?('schedule')
  raise "#{name} still has release write permissions" unless workflow['permissions']['contents'] == 'read'
  raise "#{name} still has a publisher" if workflow['jobs'].key?('publish')
end
build = File.read(File.join(root, '.github/workflows/build.yml'))
raise 'legacy release bypass remains' if build.include?('scripts/create_github_release.sh')
raise 'firmware build entrypoint missing' unless build.include?('run: ./debian_build.sh')
raise 'channel build entrypoint missing' unless build.include?('scripts/release/release.py build --plan')
raise 'production build must run the pinned ARM smoke test' unless build.include?('scripts/run_tests_in_docker.sh --production --suite production-cross-smoke')
raise 'channel firmware must validate package transactions' unless build.include?('bash scripts/test_system_config_package.sh')
raise 'package builds must validate package transactions' unless jobs['business']['steps'].any? { |step| step.fetch('run', '').include?('bash scripts/test_system_config_package.sh') }
raise 'signing key must also be rootfs trust anchor' unless build.include?('OTA_TRUST_PUBLIC_KEY_PATH=$public_key')
raise 'SDK checkout must use planned source' unless build.include?('inputs.source_ref || github.sha')
raise 'Go caches must remain cleanable' unless build.include?('chmod -R u+w "$go_mod_cache"')
ci = File.read(File.join(root, '.github/workflows/ci.yml'))
manifest = File.read(File.join(root, 'tests/test-manifest.yaml'))
raise 'CI must use the full Docker test manifest' unless ci.include?('run: make check-full')
makefile = File.read(File.join(root, 'Makefile'))
raise 'make check-full must execute the full Docker profile' unless makefile.include?('bash scripts/run_tests_in_docker.sh --profile full')
raise 'Docker test wrapper must execute the manifest runner' unless File.read(File.join(root, 'scripts/run_tests_in_docker.sh')).include?('tests/run_manifest.py')
raise 'CI must build the test image with BuildKit cache' unless ci.include?('docker/build-push-action@v6') && ci.include?('cache-from: type=gha,scope=aico-link-firmware-test')
raise 'CI must reuse the cached test image in the manifest runner' unless ci.include?('AICO_TEST_SKIP_BUILD:') && ci.include?('AICO_TEST_IMAGE: aico-link-firmware-test:ci')
raise 'CI must not duplicate the production ARM smoke suite' if ci.include?('--production --suite production-cross-smoke')
raise 'CI Docker sandbox smoke must run in the test image with host networking' unless ci.include?('--docker-socket --host-network') && ci.include?('--suite docker-sandbox-smoke')
raise 'manifest must cover channel decisions' unless manifest.include?('python3 scripts/test_channel_release.py')
RUBY

echo 'release CI script tests passed'
