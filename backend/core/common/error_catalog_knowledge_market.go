package common

import "net/http"

// Knowledge market predates the audited Core error catalog. Map its stable
// source messages to the existing shared error classes.
func init() {
	for _, source := range []string{
		"market_item_id and x-user-id are required",
		"invalid status",
		"job_id is required",
		"knowledge base has no download package configured",
		"display_name and user are required",
		"dataset and files are required",
		"invalid package url",
		"archive entry escapes destination",
		"not a git repository url",
		"zip entry is a symlink",
		"knowledge base has no package url",
		"package contains no files",
		"invalid install payload",
		"knowledge market catalog yaml path is required",
		"knowledge market catalog item requires non-empty id, name and category",
		"invalid update payload",
		"invalid update-all payload",
	} {
		registerAdditionalErrorAlias(source, "Invalid request", http.StatusBadRequest, 2000103)
	}
	registerAdditionalErrorAlias("x-user-id is required", "X-User-Id is required", http.StatusBadRequest, 2000205)

	for _, source := range []string{
		"knowledge market item not found",
		"knowledge market task not found",
		"knowledge base is not installed",
	} {
		registerAdditionalErrorAlias(source, "Resource not found", http.StatusNotFound, 2000106)
	}

	for _, source := range []string{
		"knowledge base task is running, retry later",
		"knowledge base update task is running, retry later",
	} {
		registerAdditionalErrorAlias(source, "Conflict", http.StatusConflict, 2000107)
	}

	for _, source := range []string{
		"internal server error",
		"query market install failed",
		"query active market jobs failed",
		"enqueue install job failed",
		"enqueue update job failed",
		"query active update batch failed",
		"enqueue update-all job failed",
		"query active market update batch failed",
		"delete market install failed",
		"delete market install/update jobs failed",
		"query dataset failed",
		"kb service delete failed",
		"soft delete documents failed",
		"create dataset dir failed",
		"create document/task rows failed",
		"submit parse tasks failed",
		"download retry cleanup failed",
		"download retry mkdir failed",
		"download failed after retry",
		"git rev-parse head failed",
		"lfs object checksum mismatch",
		"lfs object size mismatch",
		"download package failed",
		"check embedding model failed",
		"import files failed",
		"decode install payload",
		"check dataset documents failed",
		"check remote revision failed",
		"clear old documents failed",
		"decode update payload",
		"decode update-all payload",
	} {
		registerAdditionalErrorAlias(source, "Internal server error", http.StatusInternalServerError, 2000000)
	}

	for _, pattern := range []string{
		"dataset %s does not belong to user %s",
		"unsupported package url scheme %q",
		"invalid archive entry %q",
		"git rev-parse head returned invalid commit %q",
		"cannot parse modelscope repo path %q",
		"lfs pointer %s has no oid",
		"lfs pointer %s has invalid size",
		"lfs pointer found but host %q is not supported",
		"knowledge market catalog contains duplicate id %q",
		"knowledge market catalog item %q has invalid category %q",
	} {
		registerAdditionalErrorPattern(pattern, "Invalid request", http.StatusBadRequest, 2000103)
	}

	for _, pattern := range []string{
		"copy %s failed",
		"normalize %s failed",
		"git clone %s failed",
		"hash %s failed",
		"git ls-remote %s failed",
		"git ls-remote %s returned no commit",
		"package %s produced no files",
		"get %s failed",
		"write download %s failed",
		"open zip %s failed",
		"extract zip entry %s failed",
		"resolve lfs object %s failed",
	} {
		registerAdditionalErrorPattern(pattern, "Internal server error", http.StatusInternalServerError, 2000000)
	}
}
