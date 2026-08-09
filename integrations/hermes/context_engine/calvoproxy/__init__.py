"""CalvoProxy context-engine registration for Hermes."""

from .engine import CalvoProxyContextEngine


def register(ctx) -> None:
    ctx.register_context_engine(CalvoProxyContextEngine())


__all__ = ["CalvoProxyContextEngine", "register"]
