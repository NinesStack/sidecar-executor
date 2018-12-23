#!/bin/bash -e

# Wrapper to make an individual copy of the executor for each Mesos task. This
# allows for easy upgrading of the executor for new tasks without affecting those
# that are already running on the old version

echo "--> Executor wrapper starting up..."

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

echo "--> Copying ${SIDECAR_EXECUTOR_PATH} to sandbox ${MESOS_SANDBOX}"
executor="${MESOS_SANDBOX}/`basename ${SIDECAR_EXECUTOR_PATH}`"
cp $SIDECAR_EXECUTOR_PATH $executor
echo "--> Starting ${executor}"

function cleanup {
	# Golang template description (details here https://golang.org/pkg/text/template/):
	# // Iterate over all the container environment variables
	# {{range $i, $v := .Config.Env}}
	#     // Select the variable named "TASK_ID"
	#     {{if eq (index (split $v "=") 0) "TASK_ID"}}
	#         // ... for the container where it's set to the contents of ${TASK_ID}
	#         // note the bash string concatenation done by juxtaposition!
	#         {{if eq (index (split $v "=") 1) "'"${TASK_ID}"'"}}
	#             // Print the container ID, which is a child of $ (it resolves the root node of the docker inspect output)
	#             {{$.ID}}
	#         {{end}}
	#     {{end}}
	# {{end}}
	if [ -n "${TASK_ID}" ]; then
		# Swallow all errors returned by these calls with `|| true`
		# to bubble up the status returned by the executor
		CONTAINER_ID=$(docker ps -q | xargs docker inspect -f '{{range $i, $v := .Config.Env}}{{if eq (index (split $v "=") 0) "TASK_ID"}}{{if eq (index (split $v "=") 1) "'"${TASK_ID}"'"}}{{$.ID}}{{end}}{{end}}{{end}}') || true
		if [ -n "${CONTAINER_ID}" ]; then
			echo "Orphan container detected! Stopping ${CONTAINER_ID}"
			docker stop "${CONTAINER_ID}" || true
		fi
	fi
}
# Always run cleanup when the executor exits
trap cleanup EXIT

$executor -logtostderr=true
