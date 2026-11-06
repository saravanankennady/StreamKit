"""This package's own descriptor pool. See scripts/generate_proto.py for why it has one."""

from google.protobuf import descriptor_pb2, descriptor_pool, timestamp_pb2

POOL = descriptor_pool.DescriptorPool()
# The vendored files import these, and a new pool starts empty.
for _module in (descriptor_pb2, timestamp_pb2):
    POOL.AddSerializedFile(_module.DESCRIPTOR.serialized_pb)
