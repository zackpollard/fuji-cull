import SwiftUI
import UIKit

// TimelineCollection is the culling grid's scroll surface. It replaces a
// SwiftUI LazyVStack timeline because SwiftUI's lazy layout walks its entire
// view list per layout pass — O(24k) on a real card — and with thumbnails
// landing continuously each pass outlived the poll interval, starving the
// main thread into a permanent freeze (probe-verified: hung stacks showed
// LazyStack.sizeThatFits walking _ViewList nodes). UICollectionView
// virtualizes for real: only visible cells exist, layout comes from counts,
// and state changes reconfigure exactly the affected cells — the
// RecyclerView the Android build already relies on at this scale.
//
// Cells host SwiftUI content (UIHostingConfiguration) fed plain VALUES, so
// no cell observes the model and a model change can never fan out into a
// whole-tree invalidation.
struct TimelineCollection: UIViewRepresentable {
    @ObservedObject var model: GridModel

    static let jumpToMonth = Notification.Name("timelineJumpToMonth")

    func makeCoordinator() -> Coordinator { Coordinator(model: model) }

    func makeUIView(context: Context) -> UICollectionView {
        let cv = UICollectionView(frame: .zero, collectionViewLayout: Coordinator.makeLayout())
        cv.backgroundColor = UIColor(Color.appBG)
        cv.delegate = context.coordinator
        context.coordinator.attach(to: cv)
        return cv
    }

    func updateUIView(_ cv: UICollectionView, context: Context) {
        context.coordinator.modelDidChange()
    }

    // One section per day. Item identifiers are catalog indices.
    struct DaySection: Hashable {
        let id: String
        let dayLabel: String
        let monthLabel: String? // set on the month's first day; rendered above
        let monthID: String
    }

    @MainActor
    final class Coordinator: NSObject, UICollectionViewDelegate {
        private let model: GridModel
        private weak var cv: UICollectionView?
        private var dataSource: UICollectionViewDiffableDataSource<DaySection, Int>!
        private var sections: [DaySection] = []
        private var groupsApplied = 0 // count of groups the snapshot was built from

        // last-seen state, diffed to reconfigure only what changed
        private var lastStates: [Character] = []
        private var lastOrient: [Character] = []
        private var lastImmich: [Character] = []
        private var lastDecisions: [String: String] = [:]
        private var lastFetch: [String: String] = [:]
        private var lastCursor = -1
        private var autoscrollAt = 0

        init(model: GridModel) {
            self.model = model
            super.init()
        }

        static func makeLayout() -> UICollectionViewCompositionalLayout {
            let config = UICollectionViewCompositionalLayoutConfiguration()
            config.interSectionSpacing = 4
            return UICollectionViewCompositionalLayout(sectionProvider: { _, env in
                let width = env.container.effectiveContentSize.width
                let cols = max(4, Int(width / 150))
                let item = NSCollectionLayoutItem(layoutSize: .init(
                    widthDimension: .fractionalWidth(1.0 / CGFloat(cols)),
                    heightDimension: .fractionalHeight(1.0)))
                let row = NSCollectionLayoutGroup.horizontal(
                    layoutSize: .init(widthDimension: .fractionalWidth(1.0),
                                      heightDimension: .fractionalWidth(2.0 / 3.0 / CGFloat(cols))),
                    subitems: [item])
                row.interItemSpacing = .fixed(3)
                let section = NSCollectionLayoutSection(group: row)
                section.interGroupSpacing = 3
                section.contentInsets = .init(top: 0, leading: 3, bottom: 6, trailing: 3)
                section.boundarySupplementaryItems = [
                    NSCollectionLayoutBoundarySupplementaryItem(
                        layoutSize: .init(widthDimension: .fractionalWidth(1.0),
                                          heightDimension: .estimated(28)),
                        elementKind: UICollectionView.elementKindSectionHeader,
                        alignment: .top)
                ]
                return section
            }, configuration: config)
        }

        func attach(to cv: UICollectionView) {
            self.cv = cv

            let cell = UICollectionView.CellRegistration<UICollectionViewCell, Int> { [weak self] cell, _, index in
                guard let self, index < self.model.shots.count else { return }
                let m = self.model
                let shot = m.shots[index]
                // videos: the engine has no poster to serve on mobile — the
                // client-side poster file (Posters) is the thumbnail
                let poster = shot.kind == "video" ? Posters.shared.cached(shot) : nil
                cell.contentConfiguration = UIHostingConfiguration {
                    TileContent(
                        url: poster ?? m.thumbURL(shot.id, index),
                        cacheKey: poster != nil ? "\(shot.id):poster" : "\(shot.id):\(m.orientOf(index))",
                        ready: poster != nil || m.thumbReady(index),
                        decision: m.decisions[shot.id] ?? "",
                        inImmich: m.inImmich(index),
                        buffered: m.buffered(shot.id),
                        isVideo: shot.kind == "video",
                        isCursor: m.cursor == index)
                }
                .margins(.all, 0)
                // the thumb scales-to-fill; without this it bleeds past the
                // cell into a ragged mosaic that also buries section headers
                cell.clipsToBounds = true
                cell.contentView.clipsToBounds = true
            }

            let header = UICollectionView.SupplementaryRegistration<UICollectionViewListCell>(
                elementKind: UICollectionView.elementKindSectionHeader) { [weak self] view, _, ip in
                guard let self, ip.section < self.sections.count else { return }
                let sec = self.sections[ip.section]
                view.contentConfiguration = UIHostingConfiguration {
                    VStack(alignment: .leading, spacing: 2) {
                        if let month = sec.monthLabel {
                            Text(month)
                                .font(.system(.subheadline, design: .monospaced).weight(.semibold))
                                .foregroundStyle(Color.amber)
                                .padding(.top, 8)
                        }
                        Text(sec.dayLabel)
                            .font(.system(.caption, design: .monospaced))
                            .foregroundStyle(.secondary)
                    }
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .padding(.horizontal, 5)
                    .padding(.vertical, 4)
                }
                .margins(.all, 0)
            }

            dataSource = UICollectionViewDiffableDataSource<DaySection, Int>(collectionView: cv) {
                cv, ip, index in
                cv.dequeueConfiguredReusableCell(using: cell, for: ip, item: index)
            }
            dataSource.supplementaryViewProvider = { cv, kind, ip in
                cv.dequeueConfiguredReusableSupplementary(using: header, for: ip)
            }

            NotificationCenter.default.addObserver(forName: TimelineCollection.jumpToMonth,
                                                   object: nil, queue: .main) { [weak self] note in
                guard let id = note.object as? String else { return }
                MainActor.assumeIsolated { self?.scrollToMonth(id) }
            }
            NotificationCenter.default.addObserver(forName: DebugProbe.autoscrollTick,
                                                   object: nil, queue: .main) { [weak self] _ in
                MainActor.assumeIsolated { self?.autoscrollStep() }
            }
            NotificationCenter.default.addObserver(forName: Posters.posterReady,
                                                   object: nil, queue: .main) { [weak self] note in
                guard let id = note.object as? String else { return }
                MainActor.assumeIsolated { self?.posterLanded(id) }
            }

            modelDidChange()
        }

        /// Called on any model change: applies the section snapshot when the
        /// grouping (re)builds, otherwise reconfigures exactly the cells whose
        /// state changed since last time.
        func modelDidChange() {
            guard dataSource != nil else { return }
            if model.groups.count != groupsApplied { applySnapshot() }

            var changed = Set<Int>()
            diffChars(model.states, &lastStates, into: &changed)
            diffChars(model.orientChars, &lastOrient, into: &changed)
            diffChars(model.immichChars, &lastImmich, into: &changed)
            if model.cursor != lastCursor {
                if lastCursor >= 0 { changed.insert(lastCursor) }
                changed.insert(model.cursor)
                scrollToCursorIfHidden()
                lastCursor = model.cursor
            }
            if model.decisions != lastDecisions {
                let ids = Set(model.decisions.keys).union(lastDecisions.keys)
                    .filter { model.decisions[$0] != lastDecisions[$0] }
                for (i, s) in model.shots.enumerated() where ids.contains(s.id) { changed.insert(i) }
                lastDecisions = model.decisions
            }
            if model.fetchStates != lastFetch {
                let ids = Set(model.fetchStates.keys).union(lastFetch.keys)
                    .filter { model.fetchStates[$0] != lastFetch[$0] }
                for (i, s) in model.shots.enumerated() where ids.contains(s.id) { changed.insert(i) }
                lastFetch = model.fetchStates
            }
            guard !changed.isEmpty else { return }
            var snap = dataSource.snapshot()
            let known = Set(snap.itemIdentifiers)
            let items = changed.filter { known.contains($0) }
            guard !items.isEmpty else { return }
            snap.reconfigureItems(Array(items))
            dataSource.apply(snap, animatingDifferences: false)
        }

        private func diffChars(_ new: [Character], _ old: inout [Character], into changed: inout Set<Int>) {
            if new == old { return }
            let n = min(new.count, old.count)
            for i in 0..<n where new[i] != old[i] { changed.insert(i) }
            if new.count != old.count {
                for i in n..<max(new.count, old.count) { changed.insert(i) }
            }
            old = new
        }

        private func applySnapshot() {
            var snap = NSDiffableDataSourceSnapshot<DaySection, Int>()
            var secs: [DaySection] = []
            for month in model.groups {
                for (d, day) in month.days.enumerated() {
                    let sec = DaySection(id: day.id, dayLabel: day.label,
                                         monthLabel: d == 0 ? month.label : nil,
                                         monthID: month.id)
                    secs.append(sec)
                    snap.appendSections([sec])
                    snap.appendItems(day.cells, toSection: sec)
                }
            }
            sections = secs
            groupsApplied = model.groups.count
            idToIndex = Dictionary(uniqueKeysWithValues:
                model.shots.enumerated().map { ($0.element.id, $0.offset) })
            dataSource.apply(snap, animatingDifferences: false)
        }

        private var idToIndex: [String: Int] = [:]

        private func posterLanded(_ id: String) {
            guard let index = idToIndex[id] else { return }
            var snap = dataSource.snapshot()
            guard snap.indexOfItem(index) != nil else { return }
            snap.reconfigureItems([index])
            dataSource.apply(snap, animatingDifferences: false)
        }

        private func scrollToMonth(_ id: String) {
            guard let si = sections.firstIndex(where: { $0.monthID == id }),
                  let cv, cv.numberOfSections > si, cv.numberOfItems(inSection: si) > 0 else { return }
            cv.scrollToItem(at: IndexPath(item: 0, section: si), at: .top, animated: true)
        }

        private func scrollToCursorIfHidden() {
            guard let cv, let ip = dataSource.indexPath(for: model.cursor) else { return }
            if !cv.indexPathsForVisibleItems.contains(ip) {
                cv.scrollToItem(at: ip, at: .centeredVertically, animated: false)
            }
        }

        private func autoscrollStep() {
            guard !model.groups.isEmpty else { return }
            autoscrollAt = (autoscrollAt + 1) % model.groups.count
            DebugProbe.trace("autoscroll -> \(model.groups[autoscrollAt].id)")
            scrollToMonth(model.groups[autoscrollAt].id)
        }

        // MARK: UICollectionViewDelegate

        func collectionView(_ cv: UICollectionView, didSelectItemAt ip: IndexPath) {
            if let index = dataSource.itemIdentifier(for: ip) { model.openViewer(index) }
        }

        func collectionView(_ cv: UICollectionView, willDisplay cell: UICollectionViewCell,
                            forItemAt ip: IndexPath) {
            if let index = dataSource.itemIdentifier(for: ip) { model.hint(index) }
        }
    }
}

// TileContent is one grid thumbnail, driven entirely by values — no model
// reference, so a model change can only reach it through an explicit cell
// reconfigure.
struct TileContent: View {
    let url: URL?
    let cacheKey: String
    let ready: Bool
    let decision: String
    let inImmich: Bool
    let buffered: Bool
    let isVideo: Bool
    let isCursor: Bool

    var body: some View {
        ZStack(alignment: .bottom) {
            if let url {
                ThumbView(url: url, cacheKey: cacheKey, ready: ready)
            } else {
                Rectangle().fill(Color.white.opacity(0.04))
            }
            if decision == "keep" || decision == "reject" {
                Rectangle()
                    .fill(decision == "keep" ? Color.keepGreen : Color.rejectRed)
                    .frame(height: 5)
            }
        }
        // fill the fixed-size cell the layout hands us, never dictate size
        .frame(minWidth: 0, maxWidth: .infinity, minHeight: 0, maxHeight: .infinity)
        .clipped()
        .overlay(alignment: .topLeading) {
            if inImmich {
                Image(systemName: "cloud.fill")
                    .font(.system(size: 10))
                    .foregroundStyle(.white.opacity(0.9))
                    .padding(4)
                    .shadow(radius: 2)
            }
        }
        .overlay(alignment: .topTrailing) {
            HStack(spacing: 3) {
                if buffered {
                    Circle().fill(Color(red: 0.18, green: 0.5, blue: 0.88)).frame(width: 6, height: 6)
                }
                if isVideo {
                    Image(systemName: "play.circle.fill").foregroundStyle(.white).shadow(radius: 2)
                }
            }
            .padding(4)
        }
        .overlay(
            Rectangle().stroke(isCursor ? Color.amber : .clear, lineWidth: 2)
        )
    }
}
