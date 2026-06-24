package sample;

public class Loyalty {
    // Total loyalty points for a purchase. The points come out too high for
    // ordinary repeat purchases, but the logic is spread across Base, Tier and
    // Promo — you can't see the whole calculation in one file.
    public int points(int spend, boolean member) {
        int points = base(spend);
        points = tier(points, member);
        points = promo(points, spend);
        return points;
    }

    int base(int spend) {
        return spend / 10;
    }

    int tier(int points, boolean member) {
        return new Tier().bonus(points, member);
    }

    int promo(int points, int spend) {
        return new Promo().apply(points, spend);
    }
}
