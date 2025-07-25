.. only:: not (epub or latex or html)

    WARNING: You are looking at unreleased Cilium documentation.
    Please use the official rendered version released here:
    https://docs.cilium.io

.. _guide-to-the-hive:

Guide to the Hive
=================

Introduction
~~~~~~~~~~~~

Cilium is using dependency injection (via ``pkg/hive``) to wire up the
initialization, starting and stopping of its components. 

`Dependency injection <https://en.wikipedia.org/wiki/Dependency_injection>`_ (DI) is a
technique for separating the use of objects from their creation and
initialization. Essentially dependency injection is about automating the
manual management of dependencies. Object constructors only need to declare
their dependencies as function parameters and the rest is handled by the library. This
helps with building a loosely-coupled modular architecture as it removes the
need for centralization of initialization and configuration. It also reduces
the inclination to use global variables over explicit passing of objects,
which is often a source of bugs (due to unexpected initialization order)
and difficult to deal with in tests (as the state needs to be restored for
the next test). With dependency injection components are described as plain
values (``Cell`` in our flavor of DI) enabling visualization of inter-component
dependencies and opening the internal architecture up for inspection.

Dependency injection and the machinery described here are only a tool to
help us towards the real goal: a modular software architecture that can be
easily understood, extended, repurposed, tested and refactored by a large
group of developers with minimal overlap between modules. To achieve this we
also need to have modularity in mind when designing the architecture and APIs.

Hive and Cells
~~~~~~~~~~~~~~

Cilium applications are composed using runtime dependency injection from a set
of modular components called cells that compose together to form a hive (as in
bee hive). A hive can then be supplied with configuration and executed. To provide
a feel for what this is about, here is how a simple modular HTTP server application 
would leverage hive:

.. code-block:: go

    package server

    // The server cell implements a generic HTTP server. Provides the 'Server' API
    // for registering request handlers.
    //
    // Module() creates a named collection of cells.
    var Cell = cell.Module(
       "http-server", // Module identifier (for e.g. logging and tracing)
       "HTTP Server", // Module title (for documentation)

       // Provide the application the constructor for the server.
       cell.Provide(New),

       // Config registers a configuration when provided with the defaults 
       // and an implementation of Flags() for registering the configuration flags.
       cell.Config(defaultServerConfig),
    )

    // Server allows registering request handlers with the HTTP server
    type Server interface {
        ListenAddress() string
        RegisterHandler(path string, fn http.HandlerFunc)
    }

    func New(lc cell.Lifecycle, cfg ServerConfig) Server { 
      // Initialize http.Server, register Start and Stop hooks to Lifecycle 
      // for starting and stopping the server and return an implementation of
      // 'Server' for other cells for registering handlers.
      // ...
    }

    type ServerConfig struct {
        ServerPort uint16
    }

    var defaultServerConfig = ServerConfig{
        ServerPort: 8080,
    }

    func (def ServerConfig) Flags(flags *pflag.FlagSet) {
        // Register the "server-port" flag. Hive by convention maps the flag to the ServerPort 
        // field.
        flags.Uint16("server-port",  def.ServerPort, "Sets the HTTP server listen port")
    }

With the above generic HTTP server in the ``server`` package, we can now implement a simple handler
for /hello in the ``hello`` package:

.. code-block:: go

    package hello

    // The hello cell implements and registers a hello handler to the HTTP server.
    //
    // This cell isn't a Module, but rather just a plain Invoke. An Invoke
    // is a cell that, unlike Provide, is always executed. Invoke functions
    // can depend on values that constructors registered with Provide() can
    // return. These constructors are then called and their results remembered.
    var Cell = cell.Invoke(registerHelloHandler)

    func helloHandler(w http.ResponseWriter, req *http.Request) {
        w.Write([]byte("hello"))
    }

    func registerHelloHandler(srv server.Server) {
        srv.RegisterHandler("/hello", helloHandler)
    }
  
And then put the two together into a simple application:

.. code-block:: go

    package main

    var (
        // exampleHive is an application with an HTTP server and a handler
        // at /hello.
        exampleHive = hive.New(
            server.Cell,
            hello.Cell,
        )

        // cmd is the root command for this application. Runs
        // exampleHive when executed.
        cmd *cobra.Command = &cobra.Command{
            Use: "example",
            Run: func(cmd *cobra.Command, args []string) {
                // Run() will execute all invoke functions, followed by start hooks
                // and will then wait for interrupt signal before executing stop hooks
                // and returning.
                exampleHive.Run()
            },
        }
    )
       
    func main() {
         // Register all command-line flags from each config cell to the
         // flag-set of our command.
     	 exampleHive.RegisterFlags(cmd.Flags())

         // Add the "hive" sub-command for inspecting the application. 
         cmd.AddCommand(exampleHive.Command()))

         // Execute the root command.
         cmd.Execute()
    }


If you prefer to learn by example you can find a more complete and runnable example
application from ``pkg/hive/example``. Try running it with ``go run .`` and also try
``go run . hive``. And if you're interested in how all this is implemented internally,
see ``pkg/hive/example/mini``, a minimal example of how to do dependency injection with reflection.

The Hive API
~~~~~~~~~~~~

With the example hopefully having now whetted the appetite, we'll take a proper look at
the hive API. 

`hive <https://pkg.go.dev/github.com/cilium/hive>`_ provides the Hive type and 
`hive.New <https://pkg.go.dev/github.com/cilium/hive#New>`_ constructor. 
The ``hive.Hive`` type can be thought of as an application container, composed from cells:

.. code-block:: go

    var myHive = hive.New(foo.Cell, bar.Cell)

    // Call Run() to run the hive.     
    myHive.Run() // Start(), wait for signal (ctrl-c) and then Stop() 

    // Hive can also be started and stopped directly. Useful in tests.
    if err := myHive.Start(ctx); err != nil { /* ... */ }
    if err := myHive.Stop(ctx); err != nil { /* ... */ }

    // Hive's configuration can be registered with a Cobra command:
    hive.RegisterFlags(cmd.Flags())

    // Hive also provides a sub-command for inspecting it:
    cmd.AddCommand(hive.Command())

`hive/cell <https://pkg.go.dev/github.com/hive/cell>`_ defines the Cell interface that 
``hive.New()`` consumes and the following functions for creating cells:

- :ref:`api_module`: A named set of cells.
- :ref:`api_provide`: Provides constructor(s) to the hive.  Lazy and only invoked if referenced by an Invoke function (directly or indirectly via other constructor).
- :ref:`ProvidePrivate <api_module>`: Provides private constructor(s) to a module and its sub-modules.
- :ref:`api_decorate`: Wraps a set of cells with a decorator function to provide these cells with augmented objects.
- :ref:`api_config`: Provides a configuration struct to the hive.
- :ref:`api_invoke`: Registers an invoke function to instantiate and initialize objects.
- :ref:`api_metric`: Provides metrics to the hive.

Hive also by default provides the following globally available objects:

- :ref:`api_lifecycle`: Methods for registering Start and Stop functions that are executed when Hive is started and stopped. 
  The hooks are appended to it in dependency order (since the constructors are invoked in dependency order).
- :ref:`api_shutdowner`: Allows gracefully shutting down the hive from anywhere in case of a fatal error post-start.
- ``logrus.FieldLogger``: Interface to the logger. Module() decorates it with ``subsys=<module id>``.

.. _api_provide:

Provide
^^^^^^^

We'll now take a look at each of the different kinds of cells, starting with Provide(),
which registers one or more constructors with the hive:

.. code-block:: go

    // func Provide(ctors any...) Cell

    type A interface {}
    func NewA() A { return A{} }
    
    type B interface {}
    func NewB(A) B { return B{} }

    // simpleCell provides A and B
    var simpleCell cell.Cell = cell.Provide(NewA, NewB) 

If the constructors take many parameters, we'll want to group them into a struct with ``cell.In``,
and conversely if there are many return values, into a struct with ``cell.Out``. This tells
hive to unpack them:

.. code-block:: go

    type params struct {
    	cell.In
    
        A A
        B B
        Lifecycle cell.Lifecycle
    }
    
    type out struct {
        cell.Out
    
        C C
	D D
        E E
    }
    func NewCDE(params params) out { ... }
    
    var Cell = cell.Provide(NewCDE)

Sometimes we want to depend on a group of values sharing the same type, e.g. to collect API handlers or metrics. This can be done with 
`value groups <https://pkg.go.dev/go.uber.org/dig#hdr-Value_Groups>`_ by combining ``cell.In``
and ``cell.Out`` with the ``group`` struct tag:

.. code-block:: go

    type HandlerOut struct {
        cell.Out

        Handler Handler `group:"handlers"`
    }
    func NewHelloHandler() HandlerOut { ... }
    func NewEventHandler(src events.Source) HandlerOut { ... }

    type ServerParams struct {
        cell.In
    
        Handlers []Handler `group:"handlers"`
    }

    func NewServer(params ServerParams) Server {
      // params.Handlers will have the "Handlers" from NewHelloHandler and 
      // NewEventHandler.
    }

    var Hive = hive.New(
      cell.Provide(NewHelloHandler, NewEventHandler, NewServer)
    )

For a working example of group values this, see ``hive/example``.

Use ``Provide()`` when you want to expose an object or an interface to the application. If there is nothing meaningful
to expose, consider instead using ``Invoke()`` to register lifecycle hooks for an unexported object.

.. _api_invoke:

Invoke
^^^^^^

Invoke is used to invoke a function to initialize some part of the application. The provided constructors
won't be called unless an invoke function references them, either directly or indirectly via another
constructor:

.. code-block:: go

    // func Invoke(funcs ...any) Cell

    cell.Invoke(
        // Construct both B and C and then introduce them to each other.
        func(b B, c C) {
           b.SetHandler(c)
           c.SetOwner(b)
        },

        // Construct D for its side-effects only (e.g. start and stop hooks).
        // Avoid this if you can and use Invoke() to register hooks instead of Provide() if 
        // there's no API to provide.
        func(D){},
    )

.. _api_module:

Module
^^^^^^

Cells can be grouped into modules (a named set of cells):

.. code-block:: go

    // func Module(id, title string, cells ...Cell) Cell

    var Cell = cell.Module(
    	"example",           // short identifier (for use in e.g. logging and tracing)
	"An example module", // one-line description (for documentation)
    
        cell.Provide(New),

        innerModule,         // modules can contain other modules
    )

    var innerModule cell.Cell = cell.Module(
        "example-inner",
        "An inner module",

        cell.Provide(newInner),
    )


Module() also provides the wrapped cells with a personalized ``logrus.FieldLogger``
with the ``subsys`` field set to module identifier ("example" above).

The scope created by Module() is useful when combined with ProvidePrivate():

.. code-block:: go

    var Cell = cell.Module(
        "example",
        "An example module",
    
        cell.ProvidePrivate(NewA), // A only accessible from this module (or sub-modules)
        cell.Provide(NewB),        // B is accessible from anywhere
    )

.. _api_decorate:

Decorate
^^^^^^^^

Sometimes one may want to use a modified object inside a module, for example how above Module()
provided the cells with a personalized logger. This can be done with a decorator:

.. code-block:: go

    // func Decorate(dtor any, cells ...Cell) Cell

    var Cell = cell.Decorate(
        myLogger, // The decoration function

	// These cells will see the objects returned by the 'myLogger' decorator
        // rather than the objects on the outside.
        foo.Cell, 
        bar.Cell,
    )

    // myLogger is a decorator that can depend on one or more objects in the application
    // and return one or more objects. The input parameters don't necessarily need to match
    // the output types.
    func myLogger(log logrus.FieldLogger) logrus.FieldLogger {
        return log.WithField("lasers", "stun")
    }


.. _api_config:

Config
^^^^^^

Cilium applications use the `cobra <https://github.com/spf13/cobra>`_ and
`pflag <https://github.com/spf13/pflag>`_ libraries for implementing the command-line
interface. With Cobra, one defines a ``Command``, with optional sub-commands. Each command
has an associated FlagSet which must be populated before a command is executed in order to
parse or to produce usage documentation. Hive bridges to Cobra with ``cell.Config``, which
takes a value that implements ``cell.Flagger`` for adding flags to a command's FlagSet and
returns a cell that "provides" the parsed configuration to the application:

.. code-block:: go

    // type Flagger interface {
    //    Flags(flags *pflag.FlagSet)
    // }
    // func Config[Cfg Flagger](defaultConfig Cfg) cell.Cell

    type MyConfig struct {
        MyOption string

        SliceOption []string
        MapOption map[string]string
    }

    func (def MyConfig) Flags(flags *pflag.FlagSet) {
        // Register the "my-option" flag. This matched against the MyOption field
        // by removing any dashes and doing case insensitive comparison.
        flags.String("my-option", def.MyOption, "My config option")

        // Flags are supported for representing complex types such as slices and maps.
        // * Slices are obtained splitting the input string on commas.
        // * Maps support different formats based on how they are provided:
        //   - CLI: key=value format, separated by commas; the flag can be
        //     repeated multiple times.
        //   - Environment variable or configuration file: either JSON encoded
        //     or comma-separated key=value format.
        flags.StringSlice("slice-option", def.SliceOption, "My slice config option")
        flags.StringToString("map-option", def.MapOption, "My map config option")
    }

    var defaultMyConfig = MyConfig{
        MyOption: "the default value",
    }

    func New(cfg MyConfig) MyThing

    var Cell = cell.Module(
        "module-with-config",
        "A module with a config",

        cell.Config(defaultMyConfig),
        cell.Provide(New),
    )

Every field in the default configuration structure must be explicitly populated.
When selecting defaults for the option, consider which option will introduce
the minimal disruption to existing users during upgrade. For instance, if the
flag retains existing behavior from a previous release, then the default flag
value should retain that behavior. If you are introducing a new optional
feature, consider disabling the option by default.

In tests the configuration can be populated in various ways:

.. code-block:: go

    func TestCell(t *testing.T) {
        h := hive.New(Cell)

	// Options can be set via Viper
        h.Viper().Set("my-option", "test-value")

        // Or via pflags
        flags := pflag.NewFlagSet("", pflag.ContinueOnError)
        h.RegisterFlags(flags)
        flags.Set("my-option", "test-value")
	flags.Parse("--my-option=test-value")

	// Or the preferred way with a config override:
	h = hive.New(
            Cell,
        )
        AddConfigOverride(
            h,
            func(cfg *MyConfig) {
                cfg.MyOption = "test-override"
            })

	// To validate that the Cell can be instantiated and the configuration
        // struct is well-formed without starting you can call Populate():
        if err := h.Populate(); err != nil {
            t.Fatalf("Failed to populate: %s", err)
        }
    }

.. _api_metric:

Metric
^^^^^^

The metric cell allows you to define a collection of metrics near a feature you
would like to instrument. Like the :ref:`api_provide` cell, you define a new 
type and a constructor. In the case of a metric cell the type should be a 
struct with only public fields. The types of these fields should implement
both `metric.WithMetadata <https://pkg.go.dev/github.com/cilium/cilium/pkg/metrics/metric#WithMetadata>`_
and `prometheus.Collector <https://pkg.go.dev/github.com/prometheus/client_golang/prometheus#Collector>`_.
The easiest way to get such metrics is to use the types defined in `pkg/metrics/metric <https://pkg.go.dev/github.com/cilium/cilium/pkg/metrics/metric>`_.

The metric collection struct type returned by the given constructor is made 
available in the hive just like a normal provide. In addition all of the metrics
are made available via the ``hive-metrics`` `value group <https://pkg.go.dev/go.uber.org/dig#hdr-Value_Groups>`_.
This value group is consumed by the metrics package so any metrics defined 
via a metric cell are automatically registered.

.. code-block:: go

    var Cell = cell.Module("my-feature", "My Feature",
        cell.Metric(NewFeatureMetrics),
        cell.Provide(NewMyFeature),
    )

    type FeatureMetrics struct {
        Calls   metric.Vec[metric.Counter]
        Latency metric.Histogram
    }

    func NewFeatureMetrics() FeatureMetrics {
        return FeatureMetrics{
            Calls: metric.NewCounterVec(metric.CounterOpts{
                ConfigName: metrics.Namespace + "_my_feature_calls_total",
                Subsystem:  "my_feature",
                Namespace:  metrics.Namespace,
                Name:       "calls_total",
            }, []string{"caller"}),
            Latency: metric.NewHistogram(metric.HistogramOpts{
                ConfigName: metrics.Namespace + "_my_feature_latency_seconds",
                Namespace:  metrics.Namespace,
                Subsystem:  "my_feature",
                Name:       "latency_seconds",
            }),
        }
    }

    type MyFeature struct {
        metrics FeatureMetrics
    }

    func NewMyFeature(metrics FeatureMetrics) *MyFeature {
        return &MyFeature{
            metrics: metrics,
        }
    }

    func (mf *MyFeature) SomeFunction(caller string) {
        mf.metrics.Calls.With(prometheus.Labels{"caller": caller}).Inc()

        span := spanstat.Start()
        // Normally we would do some actual work here
        time.Sleep(time.Second)
        span.End(true)

        mf.metrics.Latency.Observe(span.Seconds())
    }

.. _api_lifecycle:

Lifecycle
^^^^^^^^^

In addition to cells an important building block in hive is the lifecycle. A
lifecycle is a list of start and stop hook pairs that are executed in order
(reverse when stopping) when running the hive.

.. code-block:: go

    package hive

    type Lifecycle {
        Append(HookInterface)
    }
    type HookContext context.Context

    type HookInterface interface {
        Start(HookContext) error
        Stop(HookContext) error
    }

    type Hook struct {
        OnStart func(HookContext) error
        OnStop func(HookContext) error
    }

    func (h Hook) Start(ctx HookContext) error { ... }
    func (h Hook) Stop(ctx HookContext) error { ... }

The lifecycle hooks can be implemented either by implementing the HookInterface methods,
or using the Hook struct. Lifecycle is accessible from any cell:

.. code-block:: go

    var ExampleCell = cell.Module(
        "example",
        "Example module",
    
        cell.Provide(New),
    )
    
    type Example struct { /* ... */ }
    func (e *Example) Start(ctx HookContext) error { /* ... */ }
    func (e *Example) Stop(ctx HookContext) error { /* ... */ }
    
    func New(lc cell.Lifecycle) *Example {
        e := &Example{}
        lc.Append(e)
        return e
    }

These hooks are executed when hive.Run() is called. The HookContext given to
these hooks is there to allow graceful aborting of the starting or stopping,
either due to user pressing ``Control-C`` or due to a timeout. By default Hive has
5 minute start timeout and 1 minute stop timeout, but these are configurable
with SetTimeouts(). A grace time of 5 seconds is given on top of the timeout
after which the application is forcefully terminated, regardless of whether
the hook has finished or not.

.. _api_shutdowner:

Shutdowner
^^^^^^^^^^

Sometimes there's nothing else to do but crash. If a fatal error is encountered
in a ``Start()`` hook it's easy: just return the error and abort the start. After
starting one can initiate a shutdown using the ``hive.Shutdowner``:

.. code-block:: go

    package hive

    type Shutdowner interface {
        Shutdown(...ShutdownOption)
    }

    func ShutdownWithError(err error) ShutdownOption { /* ... */ }

    package example

    type Example struct {
        /* ... */
        Shutdowner hive.Shutdowner
    }

    func (e *Example) eventLoop() {
        for { 
            /* ... */
            if err != nil {
                // Uh oh, this is really bad, we've got to crash.
                e.Shutdowner.Shutdown(hive.ShutdownWithError(err))
            }
        }
    }     

Creating and running a hive
~~~~~~~~~~~~~~~~~~~~~~~~~~~

A hive is created using ``hive.New()``:

.. code-block:: go

    // func New(cells ...cell.Cell) *Hive
    var myHive = hive.New(FooCell, BarCell)

``New()`` creates a new hive and registers all providers to it. Invoke
functions are not yet executed as our application may have multiple hives
and we need to delay object instantiation to until we know which hive to use.

However ``New`` does execute an invoke function to gather all command-line flags from
all configuration cells. These can be then registered with a Cobra command:

.. code-block:: go

    var cmd *cobra.Command = /* ... */
    myHive.RegisterFlags(cmd.Flags())

After that the hive can be started with ``myHive.Run()``.

Run() will first construct the parsed configurations and will then execute
all invoke functions to instantiate all needed objects. As part of this the
lifecycle hooks will have been appended (in dependency order). After that
the start hooks can be executed one after the other to start the hive. Once
started, Run() waits for SIGTERM and SIGINT signals and upon receiving one
will execute the stop hooks in reverse order to bring the hive down.

Now would be a good time to try this out in practice. You'll find a small example
application in `hive/example <https://github.com/cilium/hive/tree/main/example>`_.
Try running it with ``go run .`` and exploring the implementation (try what happens if a provider is commented out!).

Inspecting a hive
~~~~~~~~~~~~~~~~~

The ``hive.Hive`` can be inspected with the 'hive' command after it's
been registered with cobra:

.. code-block:: go

    var rootCmd *cobra.Command = /* ... */
    rootCmd.AddCommand(myHive.Command())

.. code-block:: shell-session

    cilium$ go run ./daemon hive
    Cells:

    Ⓜ️ agent (Cilium Agent):
      Ⓜ️ infra (Infrastructure):
        Ⓜ️ k8s-client (Kubernetes Client):
             ⚙️ (client.Config) {
                 K8sAPIServer: (string) "",
                 K8sKubeConfigPath: (string) "",
                 K8sClientQPS: (float32) 0,
                 K8sClientBurst: (int) 0,
                 K8sHeartbeatTimeout: (time.Duration) 30s,
                 EnableK8sAPIDiscovery: (bool) false
             }
 
             🚧 client.newClientset (cell.go:109):
                 ⇨ client.Config, cell.Lifecycle, logrus.FieldLogger 
                 ⇦ client.Clientset 
    ...

    Start hooks:

        • gops.registerGopsHooks.func1 (cell.go:44)
        • cmd.newDatapath.func1 (daemon_main.go:1625)
        ...

    Stop hooks:
        ...
   

The hive command prints out the cells, showing what modules, providers,
configurations etc. exist and what they're requiring and providing.
Finally the command prints out all registered start and stop hooks.
Note that these hooks often depend on the configuration (e.g. k8s-client
will not insert a hook unless e.g. --k8s-kubeconfig-path is given). The
hive command takes the same command-line flags as the root command.

The provider dependencies in a hive can also be visualized as a graphviz dot-graph:

.. code-block:: bash

    cilium$ go run ./daemon hive dot-graph | dot -Tx11

Guidelines
~~~~~~~~~~

Few guidelines one should strive to follow when implementing larger cells:

* A constructor function should only do validation and allocation. Spawning
  of goroutines or I/O operations must not be performed from constructors,
  but rather via the Start hook. This is required as we want to inspect the
  object graph (e.g. ``hive.PrintObjects``) and side-effectful constructors would
  cause undesired effects.

* Stop functions should make sure to block until all resources
  (goroutines, file handles, …) created by the module have been cleaned
  up (with e.g. ``sync.WaitGroup``). This makes sure that independent
  tests in the same test suite are not affecting each other. Use
  `goleak <https://github.com/uber-go/goleak>`_ to check that goroutines
  are not leaked.

* Preferably each non-trivial cell would come with a test that validates that
  it implements its public API correctly. The test also serves
  as an example of how the cell's API is used and it also validates the
  correctness of the cells  it depends on which helps with refactoring.

* Utility cells should not Invoke(). Since cells may be used in many
  applications it makes sense to make them lazy to allow bundling useful
  utilities into one collection. If a utility cell has an invoke, it may be
  instantiated even if it is never used.

* For large cells, provide interfaces and not struct pointers. A cell
  can be thought of providing a service to the rest of the application. To
  make it accessible, one should think about what APIs the module provides and
  express these as well documented interface types. If the interface is large,
  try breaking it up into multiple small ones. Interface types also allows
  integration testing with mock implementations. The rational here is the same as
  with "return structs, accept interfaces": since hive works with the names of types,
  we want to "inject" interfaces into the object graph and not struct
  pointers. Extra benefit is that separating the API implemented by a module
  into one or more interfaces it is easier to document and easier to inspect
  as all public method declarations are in one place.

* Use parameter (cell.In) and result (cell.Out) objects liberally. If a
  constructor takes more than two parameters, consider using a parameter
  struct instead.


Testing with hive script
~~~~~~~~~~~~~~~~~~~~~~~~

The hive library comes with `script <https://pkg.go.dev/github.com/cilium/hive/script>`_,
a simple scripting engine for writing tests. It is a fork of the 
`internal/script <https://github.com/golang/go/tree/master/src/cmd/internal/script>`_ library
used by the Go compiler for testing the compiler CLI usage. For usage with hive it has been
extended with support for interactive use, retrying of failures and ability to inject commands
from Hive cells. The same scripting language and commands provided by cells is available via
the ``cilium-dbg shell`` command for live inspection of the Cilium Agent.

Hive scripts are `txtar <https://pkg.go.dev/golang.org/x/tools/txtar>`_ (text archive) files
that contain a sequence of commands and a set of embedded input files. When the script is
executed a temporary directory (``$WORK``) is created and the input files are extracted
there.

To understand how this is put together, let's take a look at a minimal example:

.. literalinclude:: ../../../contrib/examples/script/example.go
   :caption: contrib/examples/script/example.go
   :language: go
   :tab-width: 4

We've now defined a module providing ``Example`` object and some commands for
interacting with it. We can now define our test runner:

.. literalinclude:: ../../../contrib/examples/script/example_test.go
   :caption: contrib/examples/script/example_test.go
   :language: go
   :tab-width: 4

And with the test runner in place we can now write our test script:

.. literalinclude:: ../../../contrib/examples/script/testdata/example.txtar
   :caption: contrib/examples/script/testdata/example.txtar
   :language: shell

With everything in place we can now run the tests:

.. code-block:: shell-session

  $ cd contrib/examples/script 
  $ go test .
  === RUN   TestScript
  === RUN   TestScript/example.txtar
    scripttest.go:251: 2025-02-26T08:32:25Z
    scripttest.go:253: $WORK=/tmp/TestScriptexample.txtar2477299450/001
    scripttest.go:72:
        DATADIR=/home/jussi/go/src/github.com/cilium/cilium/contrib/examples/script/testdata
        PWD=/tmp/TestScriptexample.txtar2477299450/001
        WORK=/tmp/TestScriptexample.txtar2477299450/001
        TMPDIR=/tmp/TestScriptexample.txtar2477299450/001/tmp

    scripttest.go:72: #! --enable-example=true
        # ^ an (optional) shebang can be used to configure cells
        # This is a comment that starts a section of commands (0.000s)
        > echo 'hello'
        [stdout]
        hello
    logger.go:256: level=INFO msg="Starting hive"
    logger.go:256: level=INFO msg="Started hive" duration=1.53µs
    scripttest.go:72: # The test hive has not been started yet, let's start it! (0.000s)
        > hive/start
    logger.go:256: level=INFO msg="SayHello() called" module=example name=foo greeting=Hello,
    scripttest.go:72: # Cells can provide custom commands (0.000s)
        > example/hello foo
        calling SayHello(foo, Hello,)
        [stdout]
        Hello, foo
        > stdout 'Hello, foo'
        matched: Hello, foo
    scripttest.go:72: # Check that call count equals 1 (0.000s)
        > example/counts
        [stdout]
        1 SayHello()
        > stdout '1 SayHello()'
        matched: 1 SayHello()
    scripttest.go:72: # The file 'foo' should not be the same as 'bar' (0.000s)
        > ! cmp foo bar
        diff foo bar
        --- foo
        +++ bar
        @@ -1,2 +1,1 @@
        -foo
        -
        +bar

    --- PASS: TestScript/example.txtar (0.00s)
    ok      github.com/cilium/cilium/contrib/examples/script        0.003s

In the test execution we can see that a temporary working directory ``$WORK`` was created
and our test files from the ``example.txtar`` extracted there. Each command was then executed
in order.

As many of the cells bring rich set of commands it's important that they're easy to discover.
To find the commands available, use the ``help`` command to interactively explore the available commands
to use in tests. Try for example adding ``break`` as the last command in ``example.txtar``:

.. code-block:: shell-session

  $ go test .
    ....
        @@ -1,2 +1,1 @@
        -foo
        -
        +bar

        > break

  Break! Control-d to continue.
  debug> help example
  [stdout]
  example/counts
          Show the call counts of the example module
  example/hello [--greeting=string] name
          Say hello
  
          Flags:
                --greeting string   Greeting to use (default "Hello,")

  debug> example/hello --greeting=Hei Jussi
  calling SayHello(Jussi, Hei)
  [stdout]
  Hei Jussi
  logger.go:256: level=INFO msg="SayHello() called" module=example name=Jussi greeting=Hei

Command reference
^^^^^^^^^^^^^^^^^

The important default commands are:

- ``help``: List available commands. Takes an optional regex to filter.
- ``hive``: Dump the hive object graph
- ``hive/start``: Start the test hive
- ``stdout regex``: Grep the stdout buffer
- ``cmp file1 file2``: Compare two files
- ``exec cmd args...``: Execute an external program (``$PATH`` needs to be set!)
- ``replace old new file``: Replace text in a file
- ``empty``: Check if file is empty

The commands can be modified with prefixes:

- ``! cmd args...``: Fail if the command succeeds
- ``* cmd args...``: Retry all commands in the section until this succeeds
- ``!* cmd args...``: Retry all commands in the section until this fails

A section is defined by a ``# comment`` line and consists of all commands between the
comment and the next comment.

New commands should use the naming scheme ``<component>/<command>``, e.g. ``hive/start`` and not
build sub-commands. This makes ``help`` more useful and makes it easier to discover the commands.

Cells with script support
^^^^^^^^^^^^^^^^^^^^^^^^^

These cells when included in the test hive will bring useful commands that can be used in tests.

- `FakeClientCell <https://github.com/cilium/cilium/blob/main/pkg/k8s/client/testutils/fake.go>`_: Commands for interacting with the fake client to add or delete objects. See ``help k8s``.
- `StateDB <https://github.com/cilium/statedb/blob/main/script.go>`_: Commands for inspecting and manipulating StateDB. Also available via ``cilium-dbg shell``. See ``help db``.
- `metrics.Cell <https://github.com/cilium/cilium/blob/main/pkg/metrics/cmd.go>`_: Commands for dumping and plotting metrics. See ``help metrics`` and ``pkg/metrics/testdata``.

Note that StateDB and metrics are part of Cilium's Hive wrapper defined in ``pkg/hive``, so if you use ``(pkg/hive).New()``
they will be included automatically.

Example tests
^^^^^^^^^^^^^

To find existing tests to use as reference you can grep for usage of scripttest.Test:

.. code-block:: shell-session

  $ git grep 'scripttest.Test'
  contrib/examples/script/example_test.go:        scripttest.Test(
  ...

Here's a few scripts that are worth calling out:

- ``daemon/k8s/testdata/pod.txtar``: Tests populating ``Table[LocalPod]`` from K8s objects defined in YAML. Good reference for the ``k8s/*`` and ``db/*`` commands.
- ``pkg/ciliumenvoyconfig/testdata``: Complex component integration tests that go from K8s objects down to BPF maps.
- ``pkg/datapath/linux/testdata/device-detection.txtar``: Low-level test that manipulates network devices in a new network namespace

Internals: Dependency injection with reflection
~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~

Hive is built on top of `uber/dig <https://github.com/uber-go/dig>`_, a reflection based library for building
dependency injection frameworks. In dig, you create a container, add in your
constructors and then "invoke" to create objects:

.. code-block:: go

    func NewA() (A, error) { /* ... */ }
    func NewB() B { /* ... */ }
    func NewC(A, B) (C, error) { /* ... */ }
    func setupC(C) error

    // Create a new container for our constructors.
    c := dig.New(dig.DeferAcyclicVerification())

    // Add in the constructors. Order does not matter.
    c.Provide(NewC)
    c.Provide(NewB)
    c.Provide(NewA)

    // Invoke a function that can depend on any of the values supplied by the
    // registered constructors.
    // Since this depends on "C", dig will construct first A and B
    // (as C depends on them), and then C.
    c.Invoke(func(c *C) {
        // Do something with C
    })


This is the basis on top of which Hive is built. Hive calls dig’s Provide()
for each of the constructors registered with cell.Provide and then calls
invoke functions to construct the needed objects. The results from the
constructors are cached, so each constructor is called only once.

``uber/dig`` uses Go’s "reflect" package that provides access to the
type information of the provide and invoke functions. For example, the
`Provide <https://pkg.go.dev/go.uber.org/dig#Container.Provide>`_ method does
something akin to this under the hood:

.. code-block:: go

    // 'constructor' has type "func(...) ..."
    typ := reflect.TypeOf(constructor)
    if typ.Kind() != reflect.Func { /* error */ }

    in := make([]reflect.Type, 0, typ.NumIn())
    for i := 0; i < typ.NumIn(); i++ { 
        in[i] = typ.In(i) 
    }

    out := make([]reflect.Type, 0, typ.NumOut())
    for i := 0; i < typ.NumOut(); i++ {
        out[i] = typ.Out(i) 
    }

    container.providers = append(container.providers, &provider{constructor, in, out})


`Invoke <https://pkg.go.dev/go.uber.org/dig#Container.Invoke>`_ will similarly
reflect on the function value to find out what are the required inputs and
then find the required constructors for the input objects and recursively
their inputs.

While building this on reflection is flexible, the downside is that missing
dependencies lead to runtime errors. Luckily dig produces excellent errors and
suggests closely matching object types in case of typos. Due to the desire
to avoid these runtime errors the constructed hive should be as static
as possible, e.g. the set of constructors and invoke functions should be
determined at compile time and not be dependent on runtime configuration. This
way the hive can be validated once with a simple unit test (``daemon/cmd/cells_test.go``).

Cell showcase
~~~~~~~~~~~~~

Logging
^^^^^^^

Logging is provided to all cells by default with the ``*slog.Logger``. The log lines will include the attribute ``module=<module id>``.

.. code-block:: go

    cell.Module(
        "example",
        "log example module",
    
        cell.Provide(
      	    func(log *slog.Logger) Example {
    	  	    log.Info("Hello") // module=example msg=Hello
                return Example{log: log}
    	    },
        ),
    )

Kubernetes client
^^^^^^^^^^^^^^^^^

The `client package <https://pkg.go.dev/github.com/cilium/cilium/pkg/k8s/client>`_ provides the ``Clientset`` API 
that combines the different clientsets used by Cilium into one composite value. Also provides ``FakeClientCell``
for writing integration tests for cells that interact with the K8s api-server.

.. code-block:: go

    var Cell = cell.Provide(New)

    func New(cs client.Clientset) Example {
         return Example{cs: cs}
    }

    func (e Example) CreateIdentity(id *ciliumv2.CiliumIdentity) error {
        return e.cs.CiliumV2().CiliumIdentities().Create(e.ctx, id, metav1.CreateOptions{})
    }

Resource and the store (see below) is the preferred way of accessing Kubernetes object
state to minimize traffic to the api-server. The Clientset should usually
only be used for creating and updating objects.

Kubernetes Resource and Store
^^^^^^^^^^^^^^^^^^^^^^^^^^^^^

.. note::

   The ``Resource[T]`` pattern is being phased out in the Cilium Agent and new code should use StateDB.
   See `daemon/k8s/tables.go <https://github.com/cilium/cilium/blob/main/daemon/k8s/tables.go>`_,
   `pkg/k8s/statedb.go <https://github.com/cilium/cilium/blob/main/pkg/k8s/statedb.go>`_
   and `PR 34060 <https://github.com/cilium/cilium/pull/34060>`_.

While not a cell by itself, `pkg/k8s/resource <https://pkg.go.dev/github.com/cilium/cilium/pkg/k8s/resource>`_ 
provides an useful abstraction for providing shared event-driven access
to Kubernetes objects. Implemented on top of the client-go informer,
``workqueue`` and store to codify the suggested pattern for controllers in a
type-safe way. This shared abstraction provides a simpler API to write and
test against and allows central control over what data (and at what rate)
is pulled from the api-server and how it’s stored (in-memory or persisted).

The resources are usually made available centrally for the application,
e.g. in cilium-agent they’re provided from `pkg/k8s/resource.go <https://github.com/cilium/cilium/blob/main/daemon/k8s/resources.go>`_.
See also the runnable example in `pkg/k8s/resource/example <https://github.com/cilium/cilium/tree/main/pkg/k8s/resource/example>`_.

.. code-block:: go

    import "github.com/cilium/cilium/pkg/k8s/resource"

    var nodesCell = cell.Provide(
        func(lc cell.Lifecycle, cs client.Clientset) resource.Resource[v1.Node] {
            lw := utils.ListerWatcherFromTyped[*v1.NodeList](cs.CoreV1().Nodes())
            return resource.New[*v1.Node](lc, lw) 
        },
    )

    var Cell = cell.Module(
        "resource-example",
        "Example of how to use Resource",

        nodesCell,
        cell.Invoke(printNodeUpdates),
    )

    func printNodeUpdates(nodes resource.Resource[*v1.Node]) {
        // Store() returns a typed locally synced store of the objects.
        // This call blocks until the store has been synchronized.
        store, err := nodes.Store()
        ...
        obj, exists, err := store.Get("my-node")
        ...
        objs, err := store.List()
        ...

        // Events() returns a channel of object change events. Closes
        // when 'ctx' is cancelled.
        // type Event[T] struct { Kind Kind; Key Key; Object T; Done func(err error) }
        for ev := range nodes.Events(ctx) {
            switch ev.Kind {
            case resource.Sync:
              // The store has now synced with api-server and
              // the set of observed upsert events forms a coherent
              // snapshot. Usually some sort of garbage collection or
              // reconciliation is performed.
            case resource.Upsert:
                fmt.Printf("Node %s has updated: %v\n", ev.Key, ev.Object)
            case resource.Delete:
                fmt.Printf("Node %s has been deleted\n", key)
            }
            // Each event must be marked as handled. If non-nil error
            // is given, the processing for this key is retried later
            // according to rate-limiting and retry policy. The built-in
            // retrying is often used if we perform I/O operations (like API client
            // calls) from the handler and retrying makes sense. It should not
            // be used on parse errors and similar.
            ev.Done(nil)
        }
    }

Job groups
^^^^^^^^^^

The `job package <https://pkg.go.dev/github.com/cilium/hive/job>`_ contains logic that
makes it easy to manage units of work that the package refers to as "jobs". These jobs are 
scheduled as part of a job group.

Every job is a callback function provided by the user with additional logic which
differs slightly for each job type. The jobs and groups manage a lot of the boilerplate
surrounding lifecycle management. The callbacks are called from the job to perform the actual
work.

These jobs themselves come in several varieties. The ``OneShot`` job invokes its callback just once.
This job type can be used for initialization after cell startup, routines that run for the full lifecycle
of the cell, or for any other task you would normally use a plain goroutine for.

The ``Timer`` job invokes its callback periodically. This job type can be used for periodic tasks
such as synchronization or garbage collection. Timer jobs can also be externally triggered in
addition to the periodic invocations.

The ``Observer`` job invokes its callback for every message sent on a ``stream.Observable``. This job
type can be used to react to a data stream or events created by other cells.

