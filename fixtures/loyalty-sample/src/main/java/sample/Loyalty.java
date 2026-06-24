package sample;

public class Loyalty {
    // points awarded for a purchase. Customers report the total comes out too
    // high for ordinary repeat purchases — the bonus seems to apply when it
    // shouldn't, but the branch logic is hard to follow by reading.
    public int points(int spend, boolean member) {
        int points = base(spend);
        if (member) {
            points = points + 5;
        }
        if (spend > 0) {
            points = points + 5;
        }
        return points;
    }

    int base(int spend) {
        return spend / 10;
    }
}
