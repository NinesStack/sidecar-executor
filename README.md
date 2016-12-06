Sidecar Executor
===============

Run Docker containers on Mesos with
[Sidecar](https://github.com/newrelic/sidecar) service discovery!

This is a Mesos executor that integrates with the service discovery platform
[Sidecar](https://github.com/newrelic/sidecar) to more tightly tie Sidecar into
the Mesos ecosystem. The main advantage is that the executor leverages the
service health checking that Sidecar already provides in order to fail Mesos
tasks quickly when they have gone off the rails.

With Sidecar and Sidecar Executor you get service health checking unified with
service Discovery, regardless of which Mesos scheduler you are running. The
system is completely scheduler agnostic.

Note that _unlike_ Sidecar, Sidecar Executor assumes that tasks are to be run
as Docker containers. You may of course still integrate non-Dockerized services
with Sidecar as normal.

Subset of Features
------------------

This executor does not attempt to support every single option that Docker can
support for running containers. It supports the core feature set that most
people actually use. If you are looking for something that it doesn't currently
provide, pull requests or feature requests are welcome.

###Currently supported:
 * Environment variables
 * Docker labels
 * Exposed port and port mappings
 * Volume binds from the host
 * Network mode setting
 * Capability Add
 * Capability Drop

This set of features probably supports most of the production containers out
there.

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
