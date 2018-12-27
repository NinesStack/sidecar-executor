=== test ===


Sidecar Executor
===============

![Travis CI](https://travis-ci.org/Nitro/sidecar-executor.svg?branch=master)

Run Docker containers on Mesos with
[Sidecar](https://github.com/newrelic/sidecar) service discovery! We're running
it with HubSpot's [Singularity](https://github.com/HubSpot/Singularity)
scheduler.

This is a Mesos executor that integrates with the service discovery platform
[Sidecar](https://github.com/newrelic/sidecar) to more tightly tie Sidecar into
the Mesos ecosystem. The main advantage is that the executor leverages the
service health checking that Sidecar already provides in order to fail Mesos
tasks quickly when they have gone off the rails.

With Sidecar and Sidecar Executor you get service health checking unified with
service Discovery, regardless of which Mesos scheduler you are running. The
system is completely scheduler agnostic to the extent possible.

Note that _unlike_ Sidecar, Sidecar Executor assumes that tasks are to be run
as Docker containers. You may of course still integrate non-Dockerized services
with Sidecar as normal.

Subset of Features
------------------

This executor does not attempt to support every single option that Docker can
support for running containers. It supports the core feature set that most
people actually use. If you are looking for something that it doesn't currently
provide, pull requests or feature requests are welcome.

### Currently supported:
 * Environment variables
 * Docker labels
 * Exposed port and port mappings
 * Volume binds from the host
 * Network mode setting
 * Capability Add
 * Capability Drop
 * Resolve environment variables stored in [Vault](https://www.vaultproject.io)
 * Enforce CPU and Memory limits via Docker cgroups

This set of features probably supports most of the production containers out
there.


Debugability
------------

This executor tries to expose as much information as possible to help with the
painful task of debugging a Mesos task failure. It logs each of the actions
that it takes and why. And, on startup it logs both the current environment
variables which the executor will run with, the settings for the executor, and
the Docker environment that will be supplied to the container. This can be
critical in understanding how a task failed.

When a task fails or is killed, the executor will fetch the last logs that were
sent to the container, and copy them to its own stdout and stderr. This means
that in most cases the Mesos logs will now contain the failure messages and
there is usually no need to dig further into logging frameworks to find out
what happened.

Additionally since each instance of the executor manages a single container,
the process name of the executor that shows up in `ps` output contains both
the ID of the Docker container and the Docker image name that was used to
start it.

Running the Executor
--------------------

A separate copy of the executor is started for each task. The binary is small
and only uses a few MB of memory so this is fairly cheap.

But, since the executor will always be running as long as your task, it will
have the file open and you won't be ablet to replace it (e.g. to upgrade) while
the task is running. It's thus recommended that you run the `executor.sh`
script as the actual executor defined in each of your tasks. That will in turn
copy the binary from a location of your choosing to the task's sandbox and then
execute it from there. This means that the copy of the executor used to start
the task will remain in the sandbox directory for the life of the task and
solves the problem nicely.

To upgrade the executor you can then simply replace the binary in the defined
location on your systems and any new tasks will start under the new copy while
older tasks remain running under their respective version. By default the
shell script assumes the path will be `/opt/mesos/sidecar-executor`.

Configuration
-------------

Each task can be run with its own executor configuration. This is helpful when
in situations such as needing to delay health checking for apps that are slow
to start, allowing for longer grace periods or a larger failure count before
shooting a container. You may configure the executor via environment variables
in the Mesos task. These will then be present for the executor at the time that
it runs. Note that these are separate from the environment variables used in
the Docker container. Currently the settings available are:

```
    Setting              Default
	------------------------------------------------------
	KillTaskTimeout:     5
	HttpTimeout:         2s
	SidecarRetryCount:   5
	SidecarRetryDelay:   3s
	SidecarUrl:          http://localhost:7777/state.json
	SidecarBackoff:      1m5s
	SidecarPollInterval: 30s
	SidecarMaxFails:     3
	SeedSidecar:         false
	DockerRepository:    https://index.docker.io/v1/
	LogsSince:           3m
	ForceCpuLimit:       false
	ForceMemoryLimit:    false
	Debug:               false
```

All of the environment variables are of the form `EXECUTOR_SIDECAR_RETRY_DELAY`
where all of the CamelCased words are split apart, and each setting is prefixed
with `EXECUTOR_`.

 * **KillTaskTimeout**: This is the amount of time to wait before we hard kill
   the container. Initially we send a SIGTERM and after this timeout we follow
   up with a SIGKILL. If your process has a clean shutdown procedure and takes
   awhile, you may want to back this off to let it complete shutdown before
   being hard killed by the kernel. **Note** that unlike the other settings,
   this, for internal library reasons, is an integer in seconds and not a Go
   time spec.

 * **HttpTimeout**: The timeout when talking to Sidecar. The default should be
   far longer than needed unless you really have something wrong.

 * **SidecarRetryCount**: This is the number of times we'll retry calling to
   Sidecar when health checking.

 * **SidecarRetryDelay**: The amount of time to wait between retries when
   contacting Sidecar.

 * **SidecarUrl**: The URL to use to contact Sidecar. The default will usually
   be the right setting.

 * **SidecarBackoff**: How long to wait before we start health checking to Sidecar.
   You want this value to be longer than the time it takes your process to start
   up and start responding as healthy on the health check endpoint.

 * **SidecarPollInterval**: The interval between asking Sidecar how healthy we
   are.

 * **SidecarMaxFails**: How many failed checks to Sidecar before we shut down
   the container? Note that this is not just _contacting_ Sidecar. This is how
   many _affirmed_ unhealthy checks we need to receive, each spaced apart by
   `SidecarPollInterval`.

 * **SeedSidecar**: Should we query the Mesos master for the list of workers
   and then provide those in the `SIDECAR_SEEDS` environment variables?

 * **DockerRepository**: This is used to match the credentials that we'll store
   from the Docker config. This will follow the same matching order as
   [described here](https://godoc.org/github.com/fsouza/go-dockerclient#NewAuthConfigurationsFromDockerCfg).
   The executor expects to use only one set of credentials for each job.

 * **LogsSince**: When the container exits or is killed, the executor will copy
   logs from the Docker container output to its own stdout and stderr so that
   they show up in the Mesos logs. `LogsSince` is how far back in time we
   reach to get these logs.

 * **ForceCpuLimit**: Should we enforce the CPU limits in the request using
   cgroups (via Docker)?

 * **ForceMemoryLimit**: Should we enforce the memory limits in the request using
   cgroups (via Docker)?

 * **Debug**: Should we turn on debug logging (verbose!) for this executor?

Configuring Docker Connectivity
-------------------------------

Sidecar Executor supports all the normal environment variables for configuring
your connection to the Docker daemon. It will look for `DOCKER_HOST`, etc in
the runtime environment and configure connectivity accordingly.

Contributing
------------

Contributions are more than welcome. Bug reports with specific reproduction
steps are great. If you have a code contribution you'd like to make, open a
pull request with suggested code.

Pull requests should:

 * Clearly state their intent in the title
 * Have a description that explains the need for the changes
 * Include tests!
 * Not break the public API

Ping us to let us know you're working on something interesting by opening a GitHub Issue on the project.
