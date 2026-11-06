"""Stands in for the livekit-local-inference package, for the oracle test only.

The official framework imports that package when it loads, and it holds models under LiveKit's Model License, which we must not
download or use outside their framework. The oracle test uses only the framework's worker and rooms, and never runs a model, so
a stand-in that does nothing lets the import succeed with no LiveKit model on this machine at all.
"""


def __getattr__(name: str):
    return type(name, (), {"__doc__": "Placeholder: no LiveKit model is present."})
