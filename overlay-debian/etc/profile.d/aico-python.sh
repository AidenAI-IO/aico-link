# Fixed pip user environment shared by the Agent service and login shells. pip
# creates its normal version-specific site-packages below this user base.
PYTHONUSERBASE=/userdata/agent/python
export PYTHONUSERBASE

export PIP_USER=1
export PIP_NO_CACHE_DIR=1
export PIP_DISABLE_PIP_VERSION_CHECK=1
export PIP_BREAK_SYSTEM_PACKAGES=1

# TMPDIR remains command-scoped to Agent Shell; systemd prepares both paths.
