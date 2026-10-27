# Lode

Lode is a self-hosted version-control project for large binary files and folders. Its intended workflow combines named branches, ordinary editable workspaces, partial checkout, and deliberate control over retained versions.

The project is currently in design. This checkout contains the [architecture outline](doc/architecture.md); it does not provide an executable or build targets.

The first interface is a CLI. A client should need only its selected files and the metadata needed to work with them. A server maintains shared history and retained contents. Files remain opaque: conflicting changes require explicit whole-file choices.

The implementation direction is one Go module and executable, with mature components supplying storage and persistence. Small, readable operations and clear state ownership guide the design.
