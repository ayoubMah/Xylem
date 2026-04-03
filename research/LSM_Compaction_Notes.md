# LSM Compaction Notes

Documenting confusions, "Aha!" moments, and other research data here.

- *Initial thought*: Compaction seems expensive. Why do it?
- *Reality*: Without it, read performance degrades linearly as the number of SSTables grows.
