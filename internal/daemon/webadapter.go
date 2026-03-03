package daemon

import (
	"github.com/jokull/udl/internal/database"
	"github.com/jokull/udl/internal/web"
)

// WebAdapter wraps the daemon Service to implement web.ServiceInterface.
// This avoids a circular import between daemon and web packages.
type WebAdapter struct {
	svc *Service
}

// NewWebAdapter creates a new adapter for the web server.
func NewWebAdapter(svc *Service) *WebAdapter {
	return &WebAdapter{svc: svc}
}

func (a *WebAdapter) StatusData() (*web.StatusData, error) {
	var reply StatusReply
	if err := a.svc.Status(&Empty{}, &reply); err != nil {
		return nil, err
	}
	return &web.StatusData{
		Running:       reply.Running,
		QueueSize:     reply.QueueSize,
		Downloading:   reply.Downloading,
		IndexerCount:  reply.IndexerCount,
		MovieCount:    reply.MovieCount,
		SeriesCount:   reply.SeriesCount,
		LibraryMovies: reply.LibraryMovies,
		LibraryTV:     reply.LibraryTV,
		FailedCount:   reply.FailedCount,
		BlockedCount:  reply.BlockedCount,
	}, nil
}

func (a *WebAdapter) QueueData() ([]database.Download, error) {
	var reply QueueReply
	if err := a.svc.Queue(&Empty{}, &reply); err != nil {
		return nil, err
	}
	return reply.Downloads, nil
}

func (a *WebAdapter) AllDownloadsData(limit int) ([]database.Download, error) {
	return a.svc.db.AllDownloads(limit)
}

func (a *WebAdapter) MovieList() ([]database.Movie, error) {
	var reply MovieListReply
	if err := a.svc.ListMovies(&Empty{}, &reply); err != nil {
		return nil, err
	}
	return reply.Movies, nil
}

func (a *WebAdapter) SeriesList() ([]database.Series, error) {
	var reply SeriesListReply
	if err := a.svc.ListSeries(&Empty{}, &reply); err != nil {
		return nil, err
	}
	return reply.Series, nil
}

func (a *WebAdapter) EpisodesForSeries(seriesID int64) ([]database.Episode, error) {
	return a.svc.db.EpisodesForSeries(seriesID)
}

func (a *WebAdapter) UpcomingEpisodes(days int) ([]database.Episode, error) {
	return a.svc.db.UpcomingEpisodes(days)
}

func (a *WebAdapter) HistoryList(limit int) ([]database.History, error) {
	return a.svc.db.ListHistory(limit)
}

func (a *WebAdapter) RetryDownload(id int64) error {
	var reply RetryDownloadReply
	return a.svc.RetryDownload(&RetryDownloadArgs{ID: id}, &reply)
}
