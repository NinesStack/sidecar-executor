Sidecar Executor
===============

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

Contributing
------------

Contributions are more than welcome. Bug reports with specific reproduction steps are great. If you have a code contribution you'd like to make, open a pull request with suggested code.

Pull requests should:

 * Clearly state their intent in the title
 * Have a description that explains the need for the changes
 * Include tests!
 * Not break the public API

Ping us to let us know you're working on something interesting by opening a GitHub Issue on the project.
