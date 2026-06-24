package sample;

public class Promo {
    // "First purchase" bonus — but the guard checks spend, not whether this is the
    // customer's first order, so the bonus is applied on EVERY purchase. This is the
    // bug that doubles the points for repeat buyers.
    int apply(int points, int spend) {
        if (spend > 0) {
            points = points + 5;
        }
        return points;
    }
}
