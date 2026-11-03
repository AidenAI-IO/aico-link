.PHONY: all configure build clean test check check-fast check-full check-docker check-production \
	test-configure test-build test-agent-webui test-clean test-production \
	sandbox-start sandbox-logs sandbox-stop

# The host is only a Docker client. Every compiler, test runner, and production
# build command runs in docker/test/Dockerfile.
all: build

configure:
	bash scripts/run_tests_in_docker.sh --production --suite production-cross-smoke

build: configure

# Local feedback should be short; CI runs check-full without omitting suites.
check:
	bash scripts/run_tests_in_docker.sh --profile quick

check-fast: check

check-full:
	bash scripts/run_tests_in_docker.sh --profile full

test: check-full

check-docker:
	bash scripts/run_tests_in_docker.sh --docker-socket --suite docker-package-contract

test-configure:
	bash scripts/run_tests_in_docker.sh --suite cpp-host

test-build:
	bash scripts/run_tests_in_docker.sh --suite cpp-host

test-agent-webui:
	bash scripts/run_tests_in_docker.sh --suite web

check-production test-production:
	bash scripts/run_tests_in_docker.sh --production --suite production-cross-smoke

test-clean:
	rm -rf build-host build-docker-host

clean:
	rm -rf build output/production-smoke

sandbox-start:
	./scripts/start_docker_sandbox.sh

sandbox-logs:
	docker compose logs -f aico

sandbox-stop:
	docker compose down
