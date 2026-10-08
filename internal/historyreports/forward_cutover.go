package historyreports

import "context"

// Unimplemented continuity API surface; see forward-continuity-contract.md.

func (s *ForwardStore) SeedFrozenForwardProfiles(ctx context.Context, r *Reader, baseDigest string) (int64, ForwardProfileSeed, error) {
	if r == nil {
		return 0, ForwardProfileSeed{}, ErrInput
	}
	_, c, err := r.ForwardMetadata(ctx)
	if err != nil {
		return 0, ForwardProfileSeed{}, err
	}
	rev, err := s.SeedForwardProfiles(ctx, c)
	return rev, ForwardProfileSeed{}, err
}

func (s *ForwardStore) PrepareForwardCutover(ctx context.Context, originDigest string, declareCoverageGap bool) (ForwardCutover, error) {
	return ForwardCutover{}, ErrSource
}

func (s *ForwardStore) PreparedCutover(ctx context.Context) (*ForwardCutover, error) { return nil, nil }

func (s *ForwardStore) ForwardSelection(ctx context.Context, bookDigest string) (ForwardSeal, error) {
	return s.NativeForwardSelection(ctx, bookDigest)
}

func (s *ForwardStore) GenerationCutover(ctx context.Context, generation string) (*ForwardCutoverStatus, error) {
	return nil, nil
}
