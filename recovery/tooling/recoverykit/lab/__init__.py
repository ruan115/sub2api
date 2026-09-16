"""Read-only checks for an explicitly identified local Docker lab endpoint."""


def inspect_lab(config):
    from .engine import UnixEngine
    from .preflight import inspect_engine

    engine = UnixEngine(config)
    result = inspect_engine(config, engine)
    engine.verify_endpoint()
    return result
