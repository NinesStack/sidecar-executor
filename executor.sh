#!/bin/bash -e

# Wrapper to make an individual copy of the executor for each Mesos task. This
# allows for easy upgrading of the executor for new tasks without affecting those
# that are already running on the old version

if [[ -z $SIDECAR_EXECUTOR_PATH ]]; then
	SIDECAR_EXECUTOR_PATH=/opt/mesos/sidecar-executor
fi

if [[ -z $SIDECAR_EXECUTOR_CUSTOM_ENV_PATH ]]; then
	SIDECAR_EXECUTOR_CUSTOM_ENV_PATH=/opt/mesos/executor-environment.sh
fi

if [ -f $SIDECAR_EXECUTOR_CUSTOM_ENV_PATH ]
then
    echo "--> Procesing custom environment at $SIDECAR_EXECUTOR_CUSTOM_ENV_PATH..."
    source $SIDECAR_EXECUTOR_CUSTOM_ENV_PATH
else
	echo "--> No custom environment found at $SIDECAR_EXECUTOR_CUSTOM_ENV_PATH"
fi

echo "--> Executor wrapper starting up..."
echo "--> Copying ${SIDECAR_EXECUTOR_PATH} to sandbox ${MESOS_SANDBOX}"
executor="${MESOS_SANDBOX}/`basename ${SIDECAR_EXECUTOR_PATH}`"
cp $SIDECAR_EXECUTOR_PATH $executor
echo "--> Starting ${executor}"
exec $executor -logtostderr=true
