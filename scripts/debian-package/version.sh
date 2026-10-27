#!/usr/bin/env bash
# Shared defaults for factory images and standalone business packages.
export AICO_BUSINESS_VERSION=${AICO_BUSINESS_VERSION:-0.0.1}
export AICO_BUSINESS_REVISION=${AICO_BUSINESS_REVISION:-2}

if [[ ! "${AICO_BUSINESS_VERSION}" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] ||
    [[ ! "${AICO_BUSINESS_REVISION}" =~ ^[1-9][0-9]*$ ]]; then
    echo 'Business version must be MAJOR.MINOR.PATCH and revision a positive integer' >&2
    return 1
fi
