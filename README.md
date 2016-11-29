Sidecar Executor
===============

WIP! This will be a Mesos executor that integrates with
Sidecar[https://github.com/newrelic/sidecar] to more tightly tie Sidecar into
the Mesos ecosystem. The main advantage is that the executor will leverage the
service health checking that Sidecar already provides in order to fail Mesos
tasks quickly when they have gone off the rails.
