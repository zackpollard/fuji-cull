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
// ScrubState is the timeline's scroll position as the date scrubber sees it:
// written by the collection coordinator (scroll fraction, scrolling flag,
// month marks at their true fractions), read by TimelineScrubber, with jump()
// going the other way. Mirrors the Android TimelineScrubber contract.
@MainActor
final class ScrubState: ObservableObject {
    @Published var fraction: Double = 0
    @Published var scrolling = false
    @Published var marks: [(frac: Double, label: String)] = []
    var jump: ((Double) -> Void)?

    /// "April 2025" -> "Apr\n25", the rail's two-line short form.
    static func short(_ label: String) -> String {
        let parts = label.split(separator: " ")
        guard parts.count == 2 else { return label }
        return "\(parts[0].prefix(3))\n\(parts[1].suffix(2))"
    }
}

struct TimelineCollection: UIViewRepresentable {
    @ObservedObject var model: GridModel
    var scrub: ScrubState? = nil

    static let jumpToMonth = Notification.Name("timelineJumpToMonth")

    func makeCoordinator() -> Coordinator { Coordinator(model: model, scrub: scrub) }

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
        private let scrub: ScrubState?
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

        init(model: GridModel, scrub: ScrubState?) {
            self.model = model
            self.scrub = scrub
            super.init()
            scrub?.jump = { [weak self] frac in self?.jump(to: frac) }
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
                        exifOrient: poster != nil ? 0 : m.orientOf(index),
                        decision: m.decisions[shot.id] ?? "",
                        inImmich: m.inImmich(index),
                        buffered: m.buffered(shot.id),
                        isVideo: shot.kind == "video",
                        isCursor: m.cursor == index,
                        hasRAF: shot.hasRAF)
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
            scrub?.marks = [] // recompute against the new layout
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

        // MARK: date scrubber plumbing

        /// Scrollable height — the denominator for every fraction.
        private func scrollableHeight(_ cv: UICollectionView) -> CGFloat {
            max(1, cv.collectionViewLayout.collectionViewContentSize.height
                - cv.bounds.height + cv.adjustedContentInset.top + cv.adjustedContentInset.bottom)
        }

        private func jump(to frac: Double) {
            guard let cv else { return }
            let y = CGFloat(frac.clamped01) * scrollableHeight(cv) - cv.adjustedContentInset.top
            cv.setContentOffset(CGPoint(x: 0, y: y), animated: false)
        }

        /// Month marks at their TRUE fraction down the timeline, from the
        /// layout's own attributes — proportional like Android's rail, not
        /// evenly spaced.
        private func computeMarksIfNeeded() {
            guard let cv, let scrub, scrub.marks.isEmpty, !sections.isEmpty else { return }
            let total = scrollableHeight(cv)
            guard cv.collectionViewLayout.collectionViewContentSize.height > cv.bounds.height else { return }
            var marks: [(Double, String)] = []
            var seenMonth = ""
            for (si, sec) in sections.enumerated() where sec.monthID != seenMonth {
                seenMonth = sec.monthID
                guard cv.numberOfSections > si, cv.numberOfItems(inSection: si) > 0,
                      let attr = cv.layoutAttributesForItem(at: IndexPath(item: 0, section: si)) else { continue }
                let label = sec.monthLabel ?? sec.dayLabel
                marks.append((Double(attr.frame.minY / total).clamped01, ScrubState.short(label)))
            }
            scrub.marks = marks
        }

        func scrollViewDidScroll(_ sv: UIScrollView) {
            guard let scrub, let cv else { return }
            computeMarksIfNeeded()
            let f = Double((sv.contentOffset.y + cv.adjustedContentInset.top) / scrollableHeight(cv)).clamped01
            if abs(f - scrub.fraction) > 0.0005 { scrub.fraction = f }
            // any motion (user, scrubber jump, programmatic) shows the rail,
            // like Android's isScrollInProgress; idle fades it out
            if !scrub.scrolling { scrub.scrolling = true }
            scheduleScrollIdle()
        }

        private var idleTask: Task<Void, Never>?
        private func scheduleScrollIdle() {
            idleTask?.cancel()
            idleTask = Task { @MainActor [weak self] in
                try? await Task.sleep(nanoseconds: 1_200_000_000)
                if !Task.isCancelled { self?.scrub?.scrolling = false }
            }
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

extension Double {
    var clamped01: Double { Swift.min(1, Swift.max(0, self)) }
}

// TimelineScrubber is the date scrubber on the timeline's right edge — the
// port of Android's Immich-style rail: month labels at their true fraction
// down the timeline (thinned so they never overlap), a draggable handle that
// tracks the scroll position, a month bubble while dragging, and it fades in
// only while the timeline is moving.
struct TimelineScrubber: View {
    @ObservedObject var scrub: ScrubState
    @GestureState private var dragging = false
    @State private var dragFrac: Double?

    private let handleSize: CGFloat = 44
    private let labelStep: CGFloat = 22

    var body: some View {
        GeometryReader { geo in
            let track = max(1, geo.size.height - handleSize)
            let frac = dragFrac ?? scrub.fraction
            ZStack(alignment: .topTrailing) {
                Color.clear
                // month labels down the rail, thinned so they never overlap
                ForEach(Array(thinned(track).enumerated()), id: \.offset) { _, mark in
                    Text(mark.label)
                        .font(.system(size: 9, design: .monospaced))
                        .foregroundStyle(.secondary)
                        .multilineTextAlignment(.trailing)
                        .padding(.trailing, 10)
                        .offset(y: CGFloat(mark.frac) * track + handleSize / 2 - 10)
                }
                // handle
                Circle()
                    .fill(Color(red: 0.23, green: 0.24, blue: 0.22))
                    .frame(width: handleSize, height: handleSize)
                    .overlay(Image(systemName: "arrow.up.and.down")
                        .font(.system(size: 15, weight: .semibold))
                        .foregroundStyle(.white))
                    .padding(.trailing, 4)
                    .offset(y: CGFloat(frac) * track)
                // month bubble while dragging
                if dragging, let f = dragFrac, let label = monthAt(f) {
                    Text(label.replacingOccurrences(of: "\n", with: " "))
                        .font(.system(size: 14, design: .monospaced))
                        .foregroundStyle(.white)
                        .padding(.horizontal, 14).padding(.vertical, 8)
                        .background(Color(red: 0.13, green: 0.15, blue: 0.13).opacity(0.95))
                        .clipShape(RoundedRectangle(cornerRadius: 14))
                        .offset(x: -56, y: CGFloat(f) * track + 4)
                }
            }
            .contentShape(Rectangle())
            .gesture(
                DragGesture(minimumDistance: 0)
                    .updating($dragging) { _, s, _ in s = true }
                    .onChanged { v in
                        let f = Double(v.location.y / geo.size.height).clamped01
                        dragFrac = f
                        scrub.jump?(f)
                    }
                    .onEnded { _ in dragFrac = nil }
            )
        }
        .frame(width: 72)
        .opacity(scrub.scrolling || dragging ? 1 : 0)
        .animation(.easeInOut(duration: 0.25), value: scrub.scrolling || dragging)
        .animation(.easeInOut(duration: 0.25), value: dragging)
    }

    private func thinned(_ track: CGFloat) -> [(frac: Double, label: String)] {
        var out: [(Double, String)] = []
        var lastY: CGFloat = -1e9
        for m in scrub.marks {
            let y = CGFloat(m.frac) * track
            if y - lastY < labelStep { continue }
            lastY = y
            out.append(m)
        }
        return out
    }

    private func monthAt(_ f: Double) -> String? {
        scrub.marks.last(where: { $0.frac <= f })?.label ?? scrub.marks.first?.label
    }
}

// TileContent is one grid thumbnail — the design system's core grammar
// (docs/design/Tile.dc.html), driven entirely by values so a model change
// can only reach it through an explicit cell reconfigure. Nothing here
// animates on data change: a keep is an instant color swap on one bar.
struct TileContent: View {
    let url: URL?
    let cacheKey: String
    let ready: Bool
    var exifOrient: Int = 0
    let decision: String
    let inImmich: Bool
    let buffered: Bool
    let isVideo: Bool
    let isCursor: Bool
    var hasRAF: Bool = false

    var body: some View {
        ZStack(alignment: .bottom) {
            if let url, ready {
                ThumbView(url: url, cacheKey: cacheKey, ready: ready, exifOrient: exifOrient)
            } else {
                // pending: diagonal stripe skeleton (static — 25k virtualized
                // cells; the design's shimmer is dropped for scroll smoothness)
                PendingStripes()
            }
            // decision bar: the glanceable edge. 6px decided; 3px faint
            // hairline for undecided so the slot reads "not yet", not empty.
            Rectangle()
                .fill(decision == "keep" ? DS.keep
                      : decision == "reject" ? DS.reject
                      : DS.undecidedHairline)
                .frame(height: decision.isEmpty ? 3 : 6)
        }
        .frame(minWidth: 0, maxWidth: .infinity, minHeight: 0, maxHeight: .infinity)
        .clipped()
        .overlay(alignment: .topLeading) {
            if hasRAF {
                Text("RAF")
                    .font(DS.micro(8))
                    .foregroundStyle(DS.text)
                    .padding(.horizontal, 4).padding(.vertical, 2)
                    .background(DS.bg.opacity(0.72))
                    .clipShape(RoundedRectangle(cornerRadius: DS.rTile))
                    .padding(DS.s1)
            }
        }
        .overlay(alignment: .topTrailing) {
            // shape-distinct marks: buffered = solid dot, Immich = hollow ring
            HStack(spacing: DS.s1) {
                if inImmich {
                    Circle().strokeBorder(DS.immich, lineWidth: 2)
                        .frame(width: 9, height: 9)
                }
                if buffered {
                    Circle().fill(DS.buffered)
                        .frame(width: 9, height: 9)
                        .overlay(Circle().stroke(DS.bg, lineWidth: 1))
                }
            }
            .padding(DS.s1)
        }
        .overlay {
            if isVideo {
                Image(systemName: "play.fill")
                    .font(.system(size: 13))
                    .foregroundStyle(DS.text)
                    .padding(9)
                    .background(DS.bg.opacity(0.5), in: Circle())
            }
        }
        .overlay {
            if isCursor {
                Rectangle()
                    .strokeBorder(DS.amber, lineWidth: 2)
                    .shadow(color: DS.amber.opacity(0.28), radius: 7)
            }
        }
    }
}

// PendingStripes is the thumb-loading skeleton: diagonal stripes on tile
// darks (design token: repeating 135° #141613 / #191C17).
struct PendingStripes: View {
    var body: some View {
        GeometryReader { geo in
            let stripe: CGFloat = 12
            let n = Int((geo.size.width + geo.size.height) / stripe) + 2
            Canvas { ctx, size in
                ctx.fill(Path(CGRect(origin: .zero, size: size)), with: .color(Color(hex: 0x141613)))
                for i in stride(from: 0, to: n, by: 2) {
                    var p = Path()
                    let x = CGFloat(i) * stripe
                    p.move(to: CGPoint(x: x, y: 0))
                    p.addLine(to: CGPoint(x: x + stripe, y: 0))
                    p.addLine(to: CGPoint(x: x + stripe - size.height, y: size.height))
                    p.addLine(to: CGPoint(x: x - size.height, y: size.height))
                    p.closeSubpath()
                    ctx.fill(p, with: .color(Color(hex: 0x191C17)))
                }
            }
        }
    }
}
