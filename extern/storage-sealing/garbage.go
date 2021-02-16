package sealing

import (
	"context"

	"golang.org/x/xerrors"
)

func (m *Sealing) PledgeSector(ctx context.Context) error {
	m.inputLk.Lock()
	defer m.inputLk.Unlock()

	cfg, err := m.getConfig()
	if err != nil {
		return xerrors.Errorf("getting config: %w", err)
	}

	if cfg.MaxSealingSectors > 0 {
		if m.stats.curSealing() >= cfg.MaxSealingSectors {
			return xerrors.Errorf("too many sectors sealing (curSealing: %d, max: %d)", m.stats.curSealing(), cfg.MaxSealingSectors)
		}
	}

	spt, err := m.currentSealProof(ctx)
	if err != nil {
		return xerrors.Errorf("getting seal proof type: %w", err)
	}

	sid, err := m.sc.Next()
	if err != nil {
		return xerrors.Errorf("generating sector number: %w", err)
	}
	sectorID := m.minerSector(spt, sid)
	err = m.sealer.NewSector(ctx, sectorID)
	if err != nil {
		return xerrors.Errorf("notifying sealer of the new sector: %w", err)
	}

	log.Infof("Creating CC sector %d", sid)
	return m.sectors.Send(uint64(sid), SectorStartCC{
		ID:         sid,
		SectorType: spt,
	})
}
