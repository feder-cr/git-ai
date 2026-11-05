# Makefile for Gitaly

# You can override options by creating a "config.mak" file in Gitaly's root
# directory.
-include config.mak

# Unexport environment variables which have an effect on Git itself.
# We need to keep GIT_PREFIX because it's used to determine where our
# self-built Git should be installed into. It's probably not going to
# matter much though.
unexport $(filter-out GIT_PREFIX,$(shell git rev-parse --local-env-vars))

# Call `make V=1` in order to print commands verbosely.
ifeq ($(V),1)
    Q =
else
    Q = @
endif

SHELL = /usr/bin/env bash -eo pipefail

# Host information
OS   := $(shell uname)
ARCH := $(shell uname -m)

# Directories
SOURCE_DIR := $(shell realpath $(abspath $(dir $(lastword ${MAKEFILE_LIST}))))
BUILD_DIR        := ${SOURCE_DIR}/_build
PROTO_DEST_DIR   := ${SOURCE_DIR}/proto/go
DEPENDENCY_DIR   := ${BUILD_DIR}/deps
TOOLS_DIR        := ${BUILD_DIR}/tools

# These variables may be overridden at runtime by top-level make
## The prefix where Gitaly binaries will be installed to. Binaries will end up
## in ${PREFIX}/bin by default.
PREFIX           ?= /usr/local
prefix           ?= ${PREFIX}
exec_prefix      ?= ${prefix}
bindir           ?= ${exec_prefix}/bin
INSTALL_DEST_DIR := ${DESTDIR}${bindir}
## The prefix where Git will be installed to.
GIT_PREFIX       ?= ${PREFIX}

# Tools
GIT                         := $(shell command -v git)
GOLANGCI_LINT               := ${TOOLS_DIR}/golangci-lint
PROTOLINT                   := ${TOOLS_DIR}/protolint
GO_LICENSES                 := ${TOOLS_DIR}/go-licenses
PROTOC                      := ${TOOLS_DIR}/protoc
PROTOC_GEN_GO               := ${TOOLS_DIR}/protoc-gen-go
PROTOC_GEN_GO_GRPC          := ${TOOLS_DIR}/protoc-gen-go-grpc
PROTOC_GEN_GITALY_LINT      := ${TOOLS_DIR}/protoc-gen-gitaly-lint
PROTOC_GEN_GITALY_PROTOLIST := ${TOOLS_DIR}/protoc-gen-gitaly-protolist
PROTOC_GEN_DOC              := ${TOOLS_DIR}/protoc-gen-doc
GOTESTSUM                   := ${TOOLS_DIR}/gotestsum
GOCOVER_COBERTURA           := ${TOOLS_DIR}/gocover-cobertura
DELVE                       := ${TOOLS_DIR}/dlv
GOVULNCHECK                 := ${TOOLS_DIR}/govulncheck

# Labkit validate-log-fields version
LABKIT_VALIDATE_VERSION     := v2.63.0

# Go toolchain options
## Ensure that newer toolchains aren't downloaded and used automatically. See the discussion
## at https://gitlab.com/gitlab-org/gitaly/-/merge_requests/7913#note_2524556226
export GOTOOLCHAIN = local

# Tool options
GOLANGCI_LINT_OPTIONS ?=
GOLANGCI_LINT_CONFIG  ?= ${SOURCE_DIR}/.golangci.yml

# Build information
GITALY_PACKAGE    := $(shell go list -m 2>/dev/null || echo unknown)
# git-describe(1) yields the tag, commit count, and short revision on a full
# clone, but fails in a shallow clone that has no reachable tag. In that case we
# fall back to the VERSION file and append the short revision ourselves so that
# "gitaly --version" still identifies the exact commit.
GITALY_VERSION    := $(shell ${GIT} describe --match v* 2>/dev/null | sed 's/^v//')
ifeq ($(strip $(GITALY_VERSION)),)
GITALY_GIT_REVISION := $(shell ${GIT} rev-parse --short HEAD 2>/dev/null)
GITALY_VERSION      := $(shell cat ${SOURCE_DIR}/VERSION 2>/dev/null || echo unknown)$(if $(GITALY_GIT_REVISION),-g$(GITALY_GIT_REVISION))
endif
GO_LDFLAGS        := -X ${GITALY_PACKAGE}/internal/version.version=${GITALY_VERSION}
SERVER_BUILD_TAGS := continuous_profiler_stackdriver

## FIPS_MODE controls whether to build Gitaly and dependencies in FIPS mode.
## Set this to a non-empty value to enable it.
FIPS_MODE ?=

ifdef FIPS_MODE
    SERVER_BUILD_TAGS := ${SERVER_BUILD_TAGS},fips

    # Build Git with the OpenSSL backend for SHA256 in case FIPS-mode is
    # requested. Note that we explicitly don't do the same for SHA1: we
    # instead use SHA1DC to protect users against the SHAttered attack.
    GIT_FIPS_MESON_BUILD_OPTIONS := -Dsha256_backend=openssl

    # The Go Cryptographic Module (GOFIPS140, Go 1.24+) is mutually exclusive
    # with GOEXPERIMENT=boringcrypto. Toolchains that select the native module
    # report its version via "go env GOFIPS140"; legacy golang-fips toolchains
    # report "off" (or empty on Go < 1.24) and still require boringcrypto.
    GOFIPS140_MODULE := $(shell go env GOFIPS140 2>/dev/null)
    ifeq ($(filter-out off,$(GOFIPS140_MODULE)),)
        # Go 1.19+ now requires GOEXPERIMENT=boringcrypto for FIPS compilation.
        # See https://github.com/golang/go/issues/51940 for more details.
        BORINGCRYPTO_SUPPORT := $(shell GOEXPERIMENT=boringcrypto go version > /dev/null 2>&1; echo $$?)
        ifeq ($(BORINGCRYPTO_SUPPORT), 0)
            export GOEXPERIMENT=boringcrypto
        endif
    endif

    export GITALY_TESTING_ENABLE_FIPS := YesPlease
endif

# protoc target
PROTOC_VERSION      ?= v30.2
PROTOC_REPO_URL     ?= https://github.com/protocolbuffers/protobuf
PROTOC_SOURCE_DIR   ?= ${DEPENDENCY_DIR}/protobuf/source
PROTOC_BUILD_DIR    ?= ${DEPENDENCY_DIR}/protobuf/build
PROTOC_INSTALL_DIR  ?= ${DEPENDENCY_DIR}/protobuf/install

ifeq ($(origin PROTOC_BUILD_OPTIONS),undefined)
    ## Build options for protoc.
    PROTOC_BUILD_OPTIONS ?=
    PROTOC_BUILD_OPTIONS += -DBUILD_SHARED_LIBS=NO
    PROTOC_BUILD_OPTIONS += -DCMAKE_INSTALL_PREFIX=${PROTOC_INSTALL_DIR}
    PROTOC_BUILD_OPTIONS += -Dprotobuf_BUILD_TESTS=OFF
    PROTOC_BUILD_OPTIONS += -DCMAKE_CXX_STANDARD=17
endif

# Factorize all protobuf includes in a single variable
PROTOC_INCLUDE ?=
PROTOC_INCLUDE += -I ${SOURCE_DIR}/proto
PROTOC_INCLUDE += -I ${PROTOC_INSTALL_DIR}/include

# Git target
GIT_REPO_URL       ?= https://gitlab.com/gitlab-org/git.git
GIT_QUIET          :=
ifeq (${Q},@)
    GIT_QUIET = --quiet
endif

GIT_EXECUTABLES += git
GIT_EXECUTABLES += git-remote-http
GIT_EXECUTABLES += git-http-backend

# == Git version configuration ==
#
# By default, Git binaries are compiled and then embedded inside of the Gitaly binary. They
# are then unpacked at runtime and executed as normal. This is known as "bundled Git", and
# is done so we maintain end-to-end control of the specific version of Git we execute at
# runtime. Multiple versions of Git can be bundled simultaneously and their usage can be
# feature-flagged.
#
# The variables below control how Git is compiled.
#
# GIT_VERSION allows a non-bundled version of Git to be used. This is defined by the nightly
# tests which exercise the `next` and `master` branches, but can also serve as an override to
# test Gitaly against any arbitrary revision in the Git source.
GIT_VERSION ?=
#
# GIT_VERSION_MASTER is a commit hash from Git’s master branch, typically between 7–14 days old.
# Do not modify the format, it's automatically updated by renovate-gitlab-bot. The timestamp
# is used for version comparison by renovate. If the version needs to be updated, the timestamp
# should be updated to the timestamp of the commit.
# renovate: 1789663697000
GIT_VERSION_MASTER ?= 3bbf43483e3954291b9fb4caa228eedb9dd528c2
#
# GIT_VERSION_PREV is the default bundled Git: the one that serves every repository except MVCC
# ones unless the git_master feature flag selects the master build. It is the release v2.56.0.
GIT_VERSION_PREV ?= a018953688f1b10bddf91bff8747068f5f4746a4
#
# GIT_VERSION_MVCC is a commit hash from Git's MVCC reference backend development branch. This branch
# diverges from upstream Git and is rebased against Git's master branch regularly. The resulting
# binary is only used for repositories that use the MVCC reference backend.
GIT_VERSION_MVCC ?= 08ed691c4d15bf3e93924c355ce2158b38a8074d
#
#
# OVERRIDE_GIT_VERSION allows you to specify a custom semver value to be reported by the
# `git --version` command. This affects bundled and non-bundled Git, and can be used whenever
# whenever the GIT_VERSION* variables are set to a revision that is not a semver value. It
# can also be left blank, and Git will compute the version during its own build.
OVERRIDE_GIT_VERSION ?=

ifeq (${GIT_VERSION:default=},)
    override GIT_VERSION := ${GIT_VERSION_PREV}
    # When GIT_VERSION is not explicitly set, we default to bundled Git.
	export WITH_BUNDLED_GIT = YesPlease
else
    # Support both vX.Y.Z and X.Y.Z version patterns, since callers across GitLab
    # use both.
    override GIT_VERSION := $(shell echo ${GIT_VERSION} | awk '/^[0-9]\.[0-9]+\.[0-9]+$$/ { printf "v" } { print $$1 }')
endif

ifeq ($(origin GIT_MESON_BUILD_OPTIONS),undefined)
    ## Build options used for Git when building with Meson.
    GIT_MESON_BUILD_OPTIONS ?=
    GIT_MESON_BUILD_OPTIONS += -Dprefix="${GIT_PREFIX}"
    GIT_MESON_BUILD_OPTIONS += -Dbuildtype=debugoptimized
    GIT_MESON_BUILD_OPTIONS += -Dcurl=enabled
    GIT_MESON_BUILD_OPTIONS += -Dexpat=disabled
    GIT_MESON_BUILD_OPTIONS += -Dgettext=disabled
    GIT_MESON_BUILD_OPTIONS += -Dgitweb=disabled
    GIT_MESON_BUILD_OPTIONS += -Diconv=enabled
    GIT_MESON_BUILD_OPTIONS += -Dpcre2=enabled
    GIT_MESON_BUILD_OPTIONS += -Dperl=disabled
    GIT_MESON_BUILD_OPTIONS += -Dpython=disabled
    # Rust is default-enabled in Git nowadays and will become mandatory with Git 3.0.
    # Disable it until our build infrastructure has Rust/Cargo available.
    GIT_MESON_BUILD_OPTIONS += -Drust=disabled
    GIT_MESON_BUILD_OPTIONS += -Dtests=false
    GIT_MESON_BUILD_OPTIONS += -Dwrap_mode=nofallback

    # Use non-collision-detecting SHA1 implementation in non-cryptographic scenarios
    # to improve performance. This is only enabled for Linux platforms.
    ifeq ($(OS),Linux)
	    GIT_MESON_BUILD_OPTIONS += -Dsha1_unsafe_backend=openssl
    endif
endif

ifdef GIT_APPEND_MESON_BUILD_OPTIONS
	GIT_MESON_BUILD_OPTIONS += ${GIT_APPEND_MESON_BUILD_OPTIONS}
endif

ifdef GIT_FIPS_MESON_BUILD_OPTIONS
	GIT_MESON_BUILD_OPTIONS += ${GIT_FIPS_MESON_BUILD_OPTIONS}
endif

# git-filter-repo target
GIT_FILTER_REPO                      ?= ${BUILD_DIR}/bin/git-filter-repo
GIT_FILTER_REPO_VERSION              ?= v2.47.0
GIT_FILTER_REPO_REPO_URL             ?= https://github.com/newren/git-filter-repo
GIT_FILTER_REPO_SOURCE_DIR           ?= ${DEPENDENCY_DIR}/git-filter-repo


# These variables control test options and artifacts
## List of Go packages which shall be tested.
## Go packages to test when using the test-go target.
TEST_PACKAGES     ?= ./...
## Test options passed to `go test`.
TEST_OPTIONS      ?= -count=1 -p=4
## Timeout of `go test` for the conformance harness (internal/graft/conformance), which CI runs in a
## step of its own, alone on the runner. It runs about 1350 subtests against real gitai servers in six
## modes, git http-backend and stock git clients: about 330 s on 4 CPUs and 530 s on 2 (measured with
## taskset on a development machine), CPU-bound, so it grows as the CPUs it gets shrink; 30m leaves about
## 3x the slower figure for a slower runner. Go's default of 10m stays the bound of every other package.
CONFORMANCE_TEST_TIMEOUT ?= 30m
## Specify the output format used to print tests ["standard-verbose", "standard-quiet", "short"]
TEST_FORMAT       ?= short
## Specify the location where the JUnit-style format shall be written to.
TEST_JUNIT_REPORT ?= ${BUILD_DIR}/reports/tests-junit.xml
## Specify the location where the full JSON report shall be written to.
TEST_JSON_REPORT  ?=
## Specify the output directory for test coverage reports.
TEST_COVERAGE_DIR ?= ${BUILD_DIR}/cover
## Directory where all runtime test data is being created.
TEST_TMP_DIR      ?=
## Custom options for gosumtest. These options are different from TEST_OPTIONS
GOTESTSUM_OPTIONS ?=
## Specify the location where gotestsum reports the tests it re-ran in CI (one line per test: runs
## and failures). Set it here rather than through GOTESTSUM_OPTIONS on the command line, which
## would replace the --rerun-fails that CI adds below.
TEST_RERUN_REPORT ?=
## Directory where Gitaly should write logs to during test execution.
TEST_LOG_DIR	  ?=
TEST_REPO_DIR     := ${BUILD_DIR}/testrepos
BENCHMARK_REPO    := ${TEST_REPO_DIR}/benchmark.git
## Options to pass to the script which builds the Gitaly gem
BUILD_GEM_OPTIONS ?=
## Options to override the name of Gitaly gem
BUILD_GEM_NAME ?= gitaly

ifdef CI
	# We introduced this flag in fb75a2f11 (test: Enable the --rerun-fails flag, 2025-03-13) and specified
	# it unconditionally. However, it's better to limit the reruns to CI only.
	GOTESTSUM_OPTIONS += --rerun-fails --packages '$(TEST_PACKAGES)'
ifneq (${TEST_RERUN_REPORT},)
	GOTESTSUM_OPTIONS += --rerun-fails-report '${TEST_RERUN_REPORT}'
endif
endif

# Git binaries that are eventually embedded into the Gitaly binary.
GIT_PACKED_EXECUTABLES       = $(addprefix ${BUILD_DIR}/bin/gitaly-, $(addsuffix -master, ${GIT_EXECUTABLES})) \
                                $(addprefix ${BUILD_DIR}/bin/gitaly-, $(addsuffix -prev, ${GIT_EXECUTABLES})) \
                                $(addprefix ${BUILD_DIR}/bin/gitaly-, $(addsuffix -mvcc, ${GIT_EXECUTABLES})) \

# All executables provided by Gitaly.
GITALY_EXECUTABLES           = $(addprefix ${BUILD_DIR}/bin/,$(notdir $(shell find ${SOURCE_DIR}/cmd -mindepth 1 -maxdepth 1 -type d -print)))
# All executables packed inside the Gitaly binary.
GITALY_PACKED_EXECUTABLES    = $(filter %gitaly-hooks %gitaly-gpg %gitaly-ssh %gitaly-lfs-smudge, ${GITALY_EXECUTABLES})

# All executables that should be installed.
GITALY_INSTALLED_EXECUTABLES = $(filter-out ${GITALY_PACKED_EXECUTABLES}, ${GITALY_EXECUTABLES})

# run_go_tests will execute Go tests with all required parameters. Its
# behaviour can be modified via the following variables:
#
# TEST_OPTIONS: any additional options
# TEST_PACKAGES: packages which shall be tested
# TEST_LOG_DIR: specify the output log dir. By default, all logs will be discarded
# Some tests build helper binaries with `go build`. The test PATH puts a guarding
# git wrapper first, which Go's VCS stamping would call, so stamping is off.
run_go_tests = PATH='${SOURCE_DIR}/internal/testhelper/testdata/home/bin:${PATH}' \
    GOFLAGS='-buildvcs=false ${GOFLAGS}' \
    TEST_TMP_DIR='${TEST_TMP_DIR}' \
    TEST_LOG_DIR='${TEST_LOG_DIR}' \
    ${GOTESTSUM} --format ${TEST_FORMAT} --junitfile '${TEST_JUNIT_REPORT}' --jsonfile '${TEST_JSON_REPORT}' ${GOTESTSUM_OPTIONS} -- -ldflags '${GO_LDFLAGS}' -tags '${SERVER_BUILD_TAGS}' ${TEST_OPTIONS} ${TEST_PACKAGES}

## Test options passed to `dlv test`.
DEBUG_OPTIONS      ?= $(patsubst -%,-test.%,${TEST_OPTIONS})

# debug_go_tests will execute Go tests from a single package in the delve debugger.
# Its behaviour can be modified via the following variable:
#
# DEBUG_OPTIONS: any additional options, will default to TEST_OPTIONS if not set.
debug_go_tests = PATH='${SOURCE_DIR}/internal/testhelper/testdata/home/bin:${PATH}' \
    TEST_TMP_DIR='${TEST_TMP_DIR}' \
    ${DELVE} test --build-flags="-ldflags '${GO_LDFLAGS}' -tags '${SERVER_BUILD_TAGS}'" ${TEST_PACKAGES} -- ${DEBUG_OPTIONS}

unexport GOROOT
## GOCACHE_MAX_SIZE_KB is the maximum size of Go's build cache in kilobytes before it is cleaned up.
GOCACHE_MAX_SIZE_KB              ?= 5000000
export GOCACHE                   ?= ${BUILD_DIR}/cache
export GOPROXY                   ?= https://proxy.golang.org|https://proxy.golang.org
export PATH                      := ${BUILD_DIR}/bin:${PATH}

# By default, intermediate targets get deleted automatically after a successful
# build. We do not want that though: there's some precious intermediate targets
# like our `*.version` targets which are required in order to determine whether
# a dependency needs to be rebuilt. By specifying `.SECONDARY`, intermediate
# targets will never get deleted automatically.
.SECONDARY:

.PHONY: all
## Default target which builds Gitaly.
all: build

.PHONY: .FORCE
.FORCE:

## Print help about available targets and variables.
help:
	@echo "usage: make [<target>...] [<variable>=<value>...]"
	@echo ""
	@echo "These are the available targets:"
	@echo ""

	@ # Match all targets which have preceding `## ` comments.
	${Q}awk '/^## / { sub(/^##/, "", $$0) ; desc = desc $$0 ; next } \
		 /^[[:alpha:]][[:alnum:]_-]+:/ && desc { print "  " $$1 desc } \
		 { desc = "" }' $(MAKEFILE_LIST) | sort | column -s: -t

	${Q}echo ""
	${Q}echo "These are common variables which can be overridden in config.mak"
	${Q}echo "or by passing them to make directly as environment variables:"
	${Q}echo ""

	@ # Match all variables which have preceding `## ` comments and which are assigned via `?=`.
	${Q}awk '/^[[:space:]]*## / { sub(/^[[:space:]]*##/,"",$$0) ; desc = desc $$0 ; next } \
		 /^[[:space:]]*[[:alpha:]][[:alnum:]_-]+[[:space:]]*\?=/ && desc { print "  "$$1 ":" desc } \
		 { desc = "" }' $(MAKEFILE_LIST) | sort | column -s: -t

.PHONY: build
## Build Go binaries.
build: ${GITALY_INSTALLED_EXECUTABLES}

.PHONY: install
## Install Gitaly binaries. The target directory can be modified by setting PREFIX and DESTDIR.
install: build
	${Q}mkdir -p ${INSTALL_DEST_DIR}
	install ${GITALY_INSTALLED_EXECUTABLES} "${INSTALL_DEST_DIR}"

.PHONY: build-bundled-git
## Build bundled Git binaries.
build-bundled-git: build-bundled-git-master build-bundled-git-prev build-bundled-git-mvcc
build-bundled-git-master: $(patsubst %,${BUILD_DIR}/bin/gitaly-%-master,${GIT_EXECUTABLES})
build-bundled-git-prev: $(patsubst %,${BUILD_DIR}/bin/gitaly-%-prev,${GIT_EXECUTABLES})
build-bundled-git-mvcc: $(patsubst %,${BUILD_DIR}/bin/gitaly-%-mvcc,${GIT_EXECUTABLES})

.PHONY: install-bundled-git
## Install bundled Git binaries. The target directory can be modified by
## setting PREFIX and DESTDIR.
install-bundled-git: install-bundled-git-master install-bundled-git-prev install-bundled-git-mvcc
install-bundled-git-master: $(patsubst %,${INSTALL_DEST_DIR}/gitaly-%-master,${GIT_EXECUTABLES})
install-bundled-git-prev: $(patsubst %,${INSTALL_DEST_DIR}/gitaly-%-prev,${GIT_EXECUTABLES})
install-bundled-git-mvcc: $(patsubst %,${INSTALL_DEST_DIR}/gitaly-%-mvcc,${GIT_EXECUTABLES})

ifdef WITH_BUNDLED_GIT
build: build-bundled-git
prepare-tests: build-bundled-git
install: install-bundled-git

else
prepare-tests: ${DEPENDENCY_DIR}/git-distribution/build/git

export GITALY_TESTING_GIT_BINARY ?= ${DEPENDENCY_DIR}/git-distribution/build/bin-wrappers/git
endif

## Enable testing with the SHA256 object format.
TEST_WITH_SHA256 ?=
ifdef TEST_WITH_SHA256
export GITALY_TEST_WITH_SHA256 = YesPlease
endif

## Enable generating test coverage.
TEST_WITH_COVERAGE ?=
ifdef TEST_WITH_COVERAGE
override TEST_OPTIONS := ${TEST_OPTIONS} -coverprofile "${TEST_COVERAGE_DIR}/all.merged"
prepare-tests: ${GOCOVER_COBERTURA} ${TEST_COVERAGE_DIR}

.PHONY: ${TEST_COVERAGE_DIR}
${TEST_COVERAGE_DIR}:
	${Q}rm -rf "${TEST_COVERAGE_DIR}"
	${Q}mkdir -p "${TEST_COVERAGE_DIR}"

# sed is used below to convert file paths to repository root relative paths.
# See https://gitlab.com/gitlab-org/gitlab/-/issues/217664
run_go_tests += \
	&& go tool cover -html  "${TEST_COVERAGE_DIR}/all.merged" -o "${TEST_COVERAGE_DIR}/all.html" \
	&& ${GOCOVER_COBERTURA} <"${TEST_COVERAGE_DIR}/all.merged" | \
	sed 's;filename=\"$(shell go list -m)/;filename=\";g' >"${TEST_COVERAGE_DIR}/cobertura.xml"
endif

.PHONY: prepare-tests
prepare-tests: ${GOTESTSUM} ${GITALY_PACKED_EXECUTABLES} ${GIT_FILTER_REPO} ${GIT_PACKED_EXECUTABLES}
	${Q}mkdir -p "$(dir ${TEST_JUNIT_REPORT})"
# The end-to-end tests build the tools of graft-agents with GOPROXY=off; that module has its own go.mod.
	${Q}go -C ${SOURCE_DIR}/tools/graft-agents mod download

.PHONY: prepare-debug
prepare-debug: ${DELVE}

.PHONY: test
## Run Go tests.
test: test-go

.PHONY: test-gitaly-linters
## Test Go tests in tools/golangci-lint/gitaly folder
## That folder has its own go.mod file. Hence tests must run the context of that folder.
test-gitaly-linters: TEST_PACKAGES := .
test-gitaly-linters: ${GOTESTSUM}
	${Q}cd ${SOURCE_DIR}/tools/golangci-lint/gitaly && $(call run_go_tests)

.PHONY: test-mod-validator
## Test Go tests in tools/mod-validator folder
## That folder has its own go.mod file. Hence tests must run the context of that folder.
test-mod-validator: TEST_PACKAGES := .
test-mod-validator: ${GOTESTSUM}
	${Q}cd ${SOURCE_DIR}/tools/mod-validator && $(call run_go_tests)

.PHONY: test-go
## Run Go tests.
test-go: prepare-tests ${GOCOVER_COBERTURA}
	${Q}$(call run_go_tests)

.PHONY: test-conformance
## Run the conformance harness: stock git clients against git http-backend and gitai in each mode,
## and the RPC tables, with the decoy tests of the operations service that it reuses.
test-conformance: TEST_PACKAGES := ./internal/graft/conformance ./internal/gitaly/service/operations
test-conformance: TEST_OPTIONS := -count=1 -timeout=${CONFORMANCE_TEST_TIMEOUT} -run '^(TestConformance.*|Test.*ResolvesBranchesExactly|Test.*ResolvesStartRepositoryBranchesExactly|TestStartBranchResolvesTheBranchFirst|TestStartBranchLocalAndRemoteAgree)$$'
test-conformance: prepare-tests
	${Q}$(call run_go_tests)

.PHONY: test-conformance-replay
## Run the conformance harness against the pre-fix commits of past bugs, in copies under
## CONFORMANCE_REPLAY_DIR, to prove that it catches them. Takes several minutes per commit.
test-conformance-replay:
	${Q}${SOURCE_DIR}/_support/conformance-replay.sh ${CONFORMANCE_REPLAY_DIR}

.PHONY: debug-test-go
## Run Go tests in delve debugger.
debug-test-go: prepare-tests prepare-debug
	${Q}$(call debug_go_tests)

.PHONY: test
## Run Go benchmarks.
bench: override TEST_FORMAT := standard-verbose
bench: override TEST_OPTIONS := -bench=. -run=^$ ${TEST_OPTIONS}
bench: ${BENCHMARK_REPO} prepare-tests
	${Q}$(call run_go_tests)

.PHONY: test-with-reftable
## Run Go tests with git's reftable backend.
## Since the reftable code isn't tagged in Git yet, to run it locally
## we have to specify the git version too:
## 'GIT_DEFAULT_REF_FORMAT=reftable OVERRIDE_GIT_VERSION="v99.99.99" GIT_VERSION="master" make test-go'
test-with-reftable: export GITALY_TEST_REF_FORMAT = reftable
test-with-reftable: test-go

.PHONY: test-with-git-master
## Run Go tests with git master.
test-with-git-master: export GITALY_TEST_GIT_MASTER = YesPlease
test-with-git-master: test-go

.PHONY: test-with-git-prev
## Run Go tests with git previous version.
test-with-git-prev: export GITALY_TEST_GIT_PREV = YesPlease
test-with-git-prev: test-go

.PHONY: test-with-sha256
## Run Go tests with SHA256 repositories.
test-with-sha256: export GITALY_TEST_WITH_SHA256 = YesPlease
test-with-sha256: test-go

.PHONY: test-with-praefect
## Run Go tests with Praefect.
test-with-praefect: export GITALY_TEST_WITH_PRAEFECT = YesPlease
test-with-praefect: test-go

.PHONY: race-go
## Run Go tests with race detection enabled.
race-go: override TEST_OPTIONS := ${TEST_OPTIONS} -race
race-go: test-go

## Running the verify steps in parallel make it difficult to read the output
## when there are failures.
.NOTPARALLEL: verify

.PHONY: verify
## Verify that various files conform to our expectations.
verify: check-mod-tidy notice-up-to-date check-proto lint

.PHONY: check-mod-tidy
check-mod-tidy:
	${Q}${GIT} diff --quiet --exit-code go.mod go.sum || (echo "error: uncommitted changes in go.mod or go.sum" && exit 1)
	${Q}go mod tidy
	${Q}${GIT} diff --quiet --exit-code go.mod go.sum || (echo "error: uncommitted changes in go.mod or go.sum" && exit 1)

${TOOLS_DIR}/gitaly-linters.so: ${SOURCE_DIR}/tools/golangci-lint/go.sum $(wildcard ${SOURCE_DIR}/tools/golangci-lint/gitaly/*.go)
	${Q}go build -buildmode=plugin -o '$@' -modfile ${SOURCE_DIR}/tools/golangci-lint/go.mod $(filter-out $<,$^)

.PHONY: lint
## Run Go linter.
lint: ${GOLANGCI_LINT} ${GITALY_PACKED_EXECUTABLES} ${GIT_PACKED_EXECUTABLES} ${TOOLS_DIR}/gitaly-linters.so lint-gitaly-linters lint-go-mod
	${Q}${GOLANGCI_LINT} run --build-tags "${SERVER_BUILD_TAGS}" --config ${GOLANGCI_LINT_CONFIG} ${GOLANGCI_LINT_OPTIONS}

.PHONY: lint-go-mod
## Lint Golang dependencies
lint-go-mod:
	${Q}cd ${SOURCE_DIR}/tools/mod-validator && go run . --file ${SOURCE_DIR}/go.mod

.PHONY: lint-fix
## Run Go linter and write back fixes to the files (not supported by all linters).
lint-fix: ${GOLANGCI_LINT} ${GITALY_PACKED_EXECUTABLES} ${GIT_PACKED_EXECUTABLES} ${TOOLS_DIR}/gitaly-linters.so
	${Q}${GOLANGCI_LINT} run --fix --build-tags "${SERVER_BUILD_TAGS}" --config ${GOLANGCI_LINT_CONFIG} ${GOLANGCI_LINT_OPTIONS}

.PHONY: lint-docs
## Run markdownlint-cli2-config to lint the documentation and lychee to check for broken links.
lint-docs:
	${Q}markdownlint-cli2 AGENTS.md CLAUDE.md README.md doc/**/*.md doc/*.md
	${Q}vale --minAlertLevel error AGENTS.md CLAUDE.md README.md doc
	${Q}lychee --version
	${Q}lychee --offline --include-fragments AGENTS.md CLAUDE.md README.md doc/**/*.md doc/*.md

.PHONY: lint-docs-fix
## Run markdownlint-cli2-config to lint and fix the documentation.
lint-docs-fix:
	${Q}markdownlint-cli2 AGENTS.md CLAUDE.md README.md doc/**.md --fix

.PHONY: lint-gitaly-linters
## Test Go tests in tools/golangci-lint/gitaly folder
## That folder has its own go.mod file. Hence linter must run the context of that folder.
lint-gitaly-linters: ${GOLANGCI_LINT} ${TOOLS_DIR}/gitaly-linters.so
	${Q}cd ${SOURCE_DIR}/tools/golangci-lint/gitaly && ${GOLANGCI_LINT} run --config ${GOLANGCI_LINT_CONFIG} .

.PHONY: validate-log-fields
## Validate logging fields using labkit's validate-log-fields tool.
validate-log-fields:
	${Q}go run gitlab.com/gitlab-org/labkit/v2/cmd/validate-log-fields@${LABKIT_VALIDATE_VERSION} .

.PHONY: format
## Run Go formatter and adjust imports.
format: ${GOLANGCI_LINT} ${TOOLS_DIR}/gitaly-linters.so
	${Q}${GOLANGCI_LINT} fmt --config ${GOLANGCI_LINT_CONFIG}

.PHONY: notice-up-to-date
notice-up-to-date: ${BUILD_DIR}/NOTICE
	${Q}${GIT} diff --exit-code ${BUILD_DIR}/NOTICE ${SOURCE_DIR}/NOTICE || (echo >&2 "NOTICE requires update: 'make notice'" && exit 1)

.PHONY: notice
## Regenerate the NOTICE file.
notice: ${SOURCE_DIR}/NOTICE

.PHONY: clean
## Clean up the build artifacts.
clean:
	rm -rf ${BUILD_DIR}

.PHONY: proto
## Regenerate protobuf definitions.
proto: ${PROTOC} ${PROTOC_GEN_GO} ${PROTOC_GEN_GO_GRPC} ${PROTOC_GEN_GITALY_PROTOLIST}
	${Q}rm -rf ${PROTO_DEST_DIR} && mkdir -p ${PROTO_DEST_DIR}/gitalypb
	${Q}${PROTOC} \
		--plugin=${PROTOC_GEN_GO} \
		--plugin=${PROTOC_GEN_GO_GRPC} \
		--plugin=${PROTOC_GEN_GITALY_PROTOLIST} \
		--go_opt=paths=source_relative \
		--go-grpc_opt=paths=source_relative \
		--go_out=${PROTO_DEST_DIR}/gitalypb \
		--gitaly-protolist_out=proto_dir=${SOURCE_DIR}/proto,gitalypb_dir=${PROTO_DEST_DIR}/gitalypb:${SOURCE_DIR} \
		--go-grpc_out=${PROTO_DEST_DIR}/gitalypb \
		${PROTOC_INCLUDE} \
		${SOURCE_DIR}/proto/*.proto \
		${SOURCE_DIR}/proto/testproto/*.proto

.PHONY: check-proto
check-proto: no-proto-changes lint-proto

.PHONY: lint-proto
lint-proto: ${PROTOC} ${PROTOLINT} ${PROTOC_GEN_GITALY_LINT} proto
	${Q}${PROTOLINT} lint -config_dir_path=${SOURCE_DIR}/proto ${SOURCE_DIR}/proto/*.proto

.PHONY: build-proto-gem
## Build the Ruby Gem that contains Gitaly's Protobuf definitions.
build-proto-gem:
	${Q}rm -rf "${BUILD_DIR}/${BUILD_GEM_NAME}.gem" && mkdir -p ${BUILD_DIR}
	${Q}rm -rf "${BUILD_DIR}/${BUILD_GEM_NAME}-gem" && mkdir -p ${BUILD_DIR}/${BUILD_GEM_NAME}-gem
	${Q}cd "${SOURCE_DIR}"/tools/protogem && bundle install
	${Q}"${SOURCE_DIR}"/tools/protogem/build-proto-gem -o "${BUILD_DIR}/${BUILD_GEM_NAME}.gem" --name ${BUILD_GEM_NAME} --working-dir ${BUILD_DIR}/${BUILD_GEM_NAME}-gem ${BUILD_GEM_OPTIONS}

.PHONY: publish-proto-gem
## Build and publish the Ruby Gem that contains Gitaly's Protobuf definitions.
publish-proto-gem: build-proto-gem
	${Q}gem push "${BUILD_DIR}/gitaly.gem"

.PHONY: build-proto-docs
## Build HTML documentation for Gitaly Protobuf definitions.
build-proto-docs: ${PROTOC} ${PROTOC_GEN_DOC}
	${Q}rm -rf ${BUILD_DIR}/proto-docs && mkdir -p ${BUILD_DIR}/proto-docs
	${Q}${PROTOC} ${PROTOC_INCLUDE} --doc_out=${BUILD_DIR}/proto-docs --doc_opt=html,index.html --plugin=protoc-gen-doc=${PROTOC_GEN_DOC} ${SOURCE_DIR}/proto/*.proto

.PHONY: build-protoset
## Build a compiled Protoset file which can be used with grpcui/grpcurl.
## https://github.com/fullstorydev/grpcui?tab=readme-ov-file#protoset-files
## Example usage: grpcui -plaintext -unix -protoset <gitaly.protoset> <gitaly.socket>
build-protoset: proto
	${Q}rm -rf ${BUILD_DIR}/protoset && mkdir -p ${BUILD_DIR}/protoset
	${Q}${PROTOC} \
		--descriptor_set_out=${BUILD_DIR}/protoset/gitaly.protoset \
		--include_imports \
		${PROTOC_INCLUDE} \
		${SOURCE_DIR}/proto/*.proto

.PHONY: govulncheck
## pipefail is set in the SHELL config, but we don't care about govulncheck's exit code here.
govulncheck: ${GOVULNCHECK}
	${Q}(${GOVULNCHECK} ./... || true) | go run ${SOURCE_DIR}/tools/govulncheck-filter/main.go

.PHONY: no-changes
no-changes:
	${Q}${GIT} diff --exit-code

.PHONY: no-proto-changes
no-proto-changes: PROTO_DEST_DIR := ${BUILD_DIR}/proto-changes
no-proto-changes: proto | ${BUILD_DIR}
	${Q}${GIT} diff --no-index --exit-code -- "${SOURCE_DIR}/proto/go" "${PROTO_DEST_DIR}"

.PHONY: dump-database-schema
## Dump the clean database schema of Praefect into a file.
dump-database-schema: build
	${Q}"${SOURCE_DIR}"/_support/generate-praefect-schema >"${SOURCE_DIR}"/_support/praefect-schema.sql

.PHONY: upgrade-module
upgrade-module:
	${Q}go run ${SOURCE_DIR}/tools/module-updater/main.go -dir . -from=${FROM_MODULE} -to=${TO_MODULE}
	${Q}${MAKE} proto

.PHONY: git
# This target is deprecated and will eventually be removed.
git: install-git

.PHONY: build-git
## Build Git distribution.
build-git: ${DEPENDENCY_DIR}/git-distribution/build/git

.PHONY: install-git
## Install Git distribution.
install-git: build-git
	${Q}meson install -C "${DEPENDENCY_DIR}/git-distribution/build"

${SOURCE_DIR}/NOTICE: ${BUILD_DIR}/NOTICE
	${Q}mv $< $@

${BUILD_DIR}/NOTICE: ${GO_LICENSES} ${GITALY_PACKED_EXECUTABLES}
	${Q}rm -rf ${BUILD_DIR}/licenses
	${Q}GOOS=linux GOFLAGS="-tags=${SERVER_BUILD_TAGS}" ${GO_LICENSES} save ${SOURCE_DIR}/... --save_path=${BUILD_DIR}/licenses
	${Q}go run ${SOURCE_DIR}/tools/noticegen/noticegen.go -source ${BUILD_DIR}/licenses -template ${SOURCE_DIR}/tools/noticegen/notice.template > ${BUILD_DIR}/NOTICE

${BUILD_DIR}:
	${Q}mkdir -p ${BUILD_DIR}
${BUILD_DIR}/bin: | ${BUILD_DIR}
	${Q}mkdir -p ${BUILD_DIR}/bin
${TOOLS_DIR}: | ${BUILD_DIR}
	${Q}mkdir -p ${TOOLS_DIR}
${DEPENDENCY_DIR}: | ${BUILD_DIR}
	${Q}mkdir -p ${DEPENDENCY_DIR}

${DEPENDENCY_DIR}/git-distribution/build/git: ${DEPENDENCY_DIR}/git-distribution/meson.build
	${Q}rm -rf "${DEPENDENCY_DIR}/git-distribution/build"
	${Q}meson setup "${DEPENDENCY_DIR}/git-distribution" "${DEPENDENCY_DIR}/git-distribution/build" ${GIT_MESON_BUILD_OPTIONS}
	${Q}meson compile -C "${DEPENDENCY_DIR}/git-distribution/build"
	${Q}touch $@

# These targets build specific releases of Git.
${BUILD_DIR}/bin/gitaly-%-prev: override GIT_VERSION = ${GIT_VERSION_PREV}
${BUILD_DIR}/bin/gitaly-%-master: override GIT_VERSION = ${GIT_VERSION_MASTER}
${BUILD_DIR}/bin/gitaly-%-mvcc: override GIT_VERSION = ${GIT_VERSION_MVCC}

${BUILD_DIR}/bin/gitaly-%-prev: ${DEPENDENCY_DIR}/git-prev/build/% | ${BUILD_DIR}/bin
	${Q}install $< $@
${BUILD_DIR}/bin/gitaly-%-master: ${DEPENDENCY_DIR}/git-master/build/% | ${BUILD_DIR}/bin
	${Q}install $< $@
${BUILD_DIR}/bin/gitaly-%-mvcc: ${DEPENDENCY_DIR}/git-mvcc/build/% | ${BUILD_DIR}/bin
	${Q}install $< $@

# clear-go-build-cache-if-needed cleans the Go build cache if it exceeds the maximum size as
# configured in GOCACHE_MAX_SIZE_KB.
.PHONY: clear-go-build-cache-if-needed
clear-go-build-cache-if-needed:
	${Q}if [ -d ${GOCACHE} ] && [ $$(du -sk ${GOCACHE} | cut -f 1) -gt ${GOCACHE_MAX_SIZE_KB} ]; then go clean --cache; fi

${BUILD_DIR}/bin/gitaly:   build-bundled-git
${BUILD_DIR}/bin/gitaly:   GO_BUILD_TAGS = ${SERVER_BUILD_TAGS}
${BUILD_DIR}/bin/gitaly:   ${GITALY_PACKED_EXECUTABLES} ${GIT_PACKED_EXECUTABLES}
${BUILD_DIR}/bin/praefect: GO_BUILD_TAGS = ${SERVER_BUILD_TAGS}

## Here we are resetting GO_BUILD_TAGS to an empty list of tags for all binaries
## except `gitaly` and `praefect`. Without this override, all binaries in
## GITALY_PACKED_EXECUTABLES will be build with SERVER_BUILD_TAGS, which we
## don't want. See MR here for more details:
## https://gitlab.com/gitlab-org/gitaly/-/merge_requests/8173
${GITALY_PACKED_EXECUTABLES}: GO_BUILD_TAGS =

${GITALY_EXECUTABLES}: ${BUILD_DIR}/bin/%: clear-go-build-cache-if-needed .FORCE
	${Q}cd ${SOURCE_DIR} && go build -o "$@" -ldflags '-B gobuildid ${GO_LDFLAGS}' -tags "${GO_BUILD_TAGS}" $(addprefix ${SOURCE_DIR}/cmd/,$(@F))

# This is a build hack to avoid excessive rebuilding of targets. Instead of
# depending on the Makefile, we start to depend on tool versions as defined in
# the Makefile. Like this, we only rebuild if the tool versions actually
# change. The dependency on the phony target is required to always rebuild
# these targets.
.PHONY: dependency-version
${DEPENDENCY_DIR}/git-%.version: dependency-version | ${DEPENDENCY_DIR}
	${Q}[ x"$$(cat "$@" 2>/dev/null)" = x"${GIT_VERSION} ${GIT_MESON_BUILD_OPTIONS}" ] || >$@ echo -n "${GIT_VERSION} ${GIT_MESON_BUILD_OPTIONS}"
${DEPENDENCY_DIR}/protoc.version: dependency-version | ${DEPENDENCY_DIR}
	${Q}[ x"$$(cat "$@" 2>/dev/null)" = x"${PROTOC_VERSION} ${PROTOC_BUILD_OPTIONS}" ] || >$@ echo -n "${PROTOC_VERSION} ${PROTOC_BUILD_OPTIONS}"
${DEPENDENCY_DIR}/git-filter-repo.version: dependency-version | ${DEPENDENCY_DIR}
	${Q}[ x"$$(cat "$@" 2>/dev/null)" = x"${GIT_FILTER_REPO_VERSION}" ] || >$@ echo -n "${GIT_FILTER_REPO_VERSION}"

# This target is responsible for checking out Git sources. In theory, we'd only
# need to depend on the source directory. But given that the source directory
# always changes when anything inside of it changes, like when we for example
# build binaries inside of it, we cannot depend on it directly or we'd
# otherwise try to rebuild all targets depending on it whenever we build
# something else. We thus depend on the meson.build file instead.
${DEPENDENCY_DIR}/git-%/meson.build: ${DEPENDENCY_DIR}/git-%.version
	${Q}${GIT} -c init.defaultBranch=master init ${GIT_QUIET} "${@D}"
	${Q}${GIT} -C "${@D}" config remote.origin.url ${GIT_REPO_URL}
	${Q}${GIT} -C "${@D}" config remote.origin.tagOpt --no-tags
	${Q}${GIT} -C "${@D}" fetch --depth 1 ${GIT_QUIET} origin ${GIT_VERSION}
	${Q}${GIT} -C "${@D}" reset --hard
	${Q}${GIT} -C "${@D}" checkout ${GIT_QUIET} --detach FETCH_HEAD
	@ # We're doing a shallow clone without any tags, so Git's default way to figure out the version via git-describe(1)
	@ # will fail. We thus have to help Git a bit: if we're asked to build from a commit directly we extract the
	@ # current version of Git from "GIT-VERSION-GEN" and append the first 8 characters of the commit ID to it.
	@ # Otherwise, we simply use the Git version passed by the user.
ifeq ($(OVERRIDE_GIT_VERSION),)
	${Q}printf "%s.g%.8s\n" "$$(sed -n '/^DEF_VER=/{s/^DEF_VER=//; s/\.GIT$$//; p; q}' <"${@D}"/GIT-VERSION-GEN)" "$$(${GIT} -C "${@D}" rev-parse --short HEAD)" >"${@D}"/version
else
	@ # We're writing the version into the "version" file in Git's own source
	@ # directory. If it exists, Git's Makefile will pick it up and use it as
	@ # the version instead of auto-detecting via git-describe(1).
	${Q}echo ${OVERRIDE_GIT_VERSION} >"${@D}"/version
endif
	${Q}touch $@

$(patsubst %,${DEPENDENCY_DIR}/git-\%/build/%,${GIT_EXECUTABLES}): ${DEPENDENCY_DIR}/git-%/meson.build
	${Q}rm -rf "$(dir ${@D})"/build
	${Q}meson setup "$(dir ${@D})" "$(dir ${@D})"/build ${GIT_MESON_BUILD_OPTIONS}
	${Q}meson compile -C "$(dir ${@D})/build" $(patsubst %,%:executable,${GIT_EXECUTABLES})
	${Q}touch $@

${INSTALL_DEST_DIR}/gitaly-%: ${BUILD_DIR}/bin/gitaly-%
	${Q}mkdir -p ${@D}
	${Q}install $< $@

${PROTOC}: ${DEPENDENCY_DIR}/protoc.version | ${TOOLS_DIR}
	${Q}${GIT} -c init.defaultBranch=master init ${GIT_QUIET} "${PROTOC_SOURCE_DIR}"
	${Q}${GIT} -C "${PROTOC_SOURCE_DIR}" config remote.origin.url ${PROTOC_REPO_URL}
	${Q}${GIT} -C "${PROTOC_SOURCE_DIR}" config remote.origin.tagOpt --no-tags
	${Q}${GIT} -C "${PROTOC_SOURCE_DIR}" fetch --depth 1 ${GIT_QUIET} origin ${PROTOC_VERSION}
	${Q}${GIT} -C "${PROTOC_SOURCE_DIR}" checkout ${GIT_QUIET} --detach FETCH_HEAD
	${Q}${GIT} -C "${PROTOC_SOURCE_DIR}" submodule update --init --recursive
	${Q}rm -rf "${PROTOC_BUILD_DIR}"
	${Q}cmake "${PROTOC_SOURCE_DIR}" -B "${PROTOC_BUILD_DIR}" ${PROTOC_BUILD_OPTIONS}
	${Q}cmake --build "${PROTOC_BUILD_DIR}" --target install -- -j $(shell nproc)
	${Q}cp "${PROTOC_INSTALL_DIR}"/bin/protoc ${PROTOC}


${PROTOC_GEN_GITALY_LINT}: proto | ${TOOLS_DIR}
	${Q}go build -o $@ ${SOURCE_DIR}/tools/protoc-gen-gitaly-lint

${PROTOC_GEN_GITALY_PROTOLIST}: | ${TOOLS_DIR}
	${Q}go build -o $@ ${SOURCE_DIR}/tools/protoc-gen-gitaly-protolist

${GIT_FILTER_REPO}: ${DEPENDENCY_DIR}/git-filter-repo.version | ${BUILD_DIR}/bin
	${Q}${GIT} -c init.defaultBranch=master init ${GIT_QUIET} "${GIT_FILTER_REPO_SOURCE_DIR}"
	${Q}${GIT} -C "${GIT_FILTER_REPO_SOURCE_DIR}" config remote.origin.url ${GIT_FILTER_REPO_REPO_URL}
	${Q}${GIT} -C "${GIT_FILTER_REPO_SOURCE_DIR}" config remote.origin.tagOpt --no-tags
	${Q}${GIT} -C "${GIT_FILTER_REPO_SOURCE_DIR}" fetch --depth 1 ${GIT_QUIET} origin ${GIT_FILTER_REPO_VERSION}
	${Q}${GIT} -C "${GIT_FILTER_REPO_SOURCE_DIR}" reset --hard
	${Q}${GIT} -C "${GIT_FILTER_REPO_SOURCE_DIR}" checkout ${GIT_QUIET} --detach FETCH_HEAD
	${Q}cp "${GIT_FILTER_REPO_SOURCE_DIR}/git-filter-repo" ${GIT_FILTER_REPO}

# External tools
${TOOLS_DIR}/%: ${SOURCE_DIR}/tools/%/tool.go ${SOURCE_DIR}/tools/%/go.mod ${SOURCE_DIR}/tools/%/go.sum | ${TOOLS_DIR}
	${Q}GOBIN=${TOOLS_DIR} go install -modfile ${SOURCE_DIR}/tools/$*/go.mod ${TOOL_PACKAGE}

${GOCOVER_COBERTURA}: TOOL_PACKAGE = github.com/t-yuki/gocover-cobertura
${GOLANGCI_LINT}:     TOOL_PACKAGE = github.com/golangci/golangci-lint/v2/cmd/golangci-lint
${PROTOLINT}:         TOOL_PACKAGE = github.com/yoheimuta/protolint/cmd/protolint
${GOTESTSUM}:         TOOL_PACKAGE = gotest.tools/gotestsum
${GO_LICENSES}:       TOOL_PACKAGE = github.com/google/go-licenses
${PROTOC_GEN_GO}:     TOOL_PACKAGE = google.golang.org/protobuf/cmd/protoc-gen-go
${PROTOC_GEN_GO_GRPC}:TOOL_PACKAGE = google.golang.org/grpc/cmd/protoc-gen-go-grpc
${PROTOC_GEN_DOC}:    TOOL_PACKAGE = github.com/pseudomuto/protoc-gen-doc/cmd/protoc-gen-doc
${DELVE}:             TOOL_PACKAGE = github.com/go-delve/delve/cmd/dlv
${GOVULNCHECK}:       TOOL_PACKAGE = golang.org/x/vuln/cmd/govulncheck
${GOVULNCHECK}:       TOOL_PACKAGE = golang.org/x/vuln/cmd/govulncheck

${BENCHMARK_REPO}:
	${GIT} clone --mirror ${GIT_QUIET} https://gitlab.com/gitlab-org/gitlab.git $@
