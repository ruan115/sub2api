"""Workspace snapshot errors do not include source contents or Git stderr."""


class WorkspaceError(ValueError):
    """A complete, private workspace snapshot could not be produced or verified."""
