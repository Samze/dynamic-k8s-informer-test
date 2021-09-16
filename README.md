# A prototype controller that manages dynamic informers

An improvement on https://github.com/Samze/dynamicreconcilerwithcancel/

A controller that spins up informers that watch for types dynamically.
These dynamic types are specified in the `ResourceRef` resource via their GVR.
Importantly these informers are managed and are created/deleted to avoid memory
leaks.

Benefits over the previous approach linked above:
* Only one informer per GVK - less memory/open connections.
* Only one controller.Manager - less overhead.